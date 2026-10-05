// SPDX-License-Identifier: AGPL-3.0-only
package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/djosh34/s3-smb/internal/backup"
	"github.com/djosh34/s3-smb/internal/config"
	"github.com/djosh34/s3-smb/internal/juicefs/pkg/meta"
	"github.com/djosh34/s3-smb/internal/juicefs/pkg/object"
	"github.com/djosh34/s3-smb/internal/smbfs"
	"github.com/djosh34/s3-smb/internal/storage"
)

type smbServer interface {
	Serve(ctx context.Context, listener net.Listener) error
	Shutdown(ctx context.Context) error
}

type smbAdapter interface {
	Shutdown() error
}

// resources holds what serve opened, in the order it was opened. After a failed
// or stuck close the state lock stays held and the process exits.
type resources struct {
	raw          object.ObjectStorage
	metadata     meta.Meta
	adapter      smbAdapter
	server       smbServer
	listener     net.Listener
	lock         *stateLock
	runtime      *storage.Runtime
	serveDone    <-chan error
	protection   *backup.Protection
	manager      *backup.Manager
	cancelBackup context.CancelFunc
	backupDone   <-chan error
	session      bool
}

// volume is the dataset serve found in the bucket and the state directory.
type volume struct {
	format *meta.Format
	// point is set when the identity came from the newest metadata backup.
	point       *backup.Point
	fresh       bool
	localExists bool
}

// serve runs the SMB service until ctx ends or a part of it fails. It calls
// hardExit when shutdown takes longer than shutdownTimeout.
func serve(ctx context.Context, c *config.Resolved, hardExit func()) (result error) {
	var r resources
	defer func() {
		timer := time.AfterFunc(shutdownTimeout, hardExit)
		defer timer.Stop()
		// A failed close may leave a writer running. The state lock stays held
		// until the process exits, so no second process can start meanwhile.
		if err := r.close(ctx); err != nil {
			result = errors.Join(result, fmt.Errorf("shutdown failed; terminating with state lock retained: %w", err))
		}
	}()
	if err := r.open(ctx, c); err != nil {
		return err
	}
	return r.run(ctx, c.SMB.ReadOnly)
}

// open starts everything serve needs and records each resource in r, so close
// can release it after a failure.
func (r *resources) open(ctx context.Context, c *config.Resolved) error {
	var err error
	r.lock, err = lockState(c.Storage.StateDir)
	if err != nil {
		return err
	}
	if !c.SMB.ReadOnly {
		// A metadata backup may take as long as the backup interval.
		r.protection, err = backup.NewProtection(c.Backup.Interval, c.Backup.Interval, c.Backup.TrashDays)
		if err != nil {
			return err
		}
	}
	r.raw, err = storage.OpenS3(c)
	if err != nil {
		return err
	}
	dbPath := filepath.Join(c.Storage.StateDir, "metadata.db")
	v, err := findVolume(ctx, r.raw, c, dbPath)
	if err != nil {
		return err
	}
	blob, err := storage.OpenVolume(ctx, r.raw, v.format, c.Passphrase, v.fresh)
	if err != nil {
		return err
	}
	recovered := false
	if !v.fresh {
		if err = verifyRemoteMarker(ctx, blob, v.format, v.point != nil, c.Storage.StateDir); err != nil {
			return err
		}
		if !v.localExists {
			if err = recoverLocal(ctx, blob, c, v.format, dbPath); err != nil {
				return err
			}
			recovered = true
		}
	}
	if err = r.openMetadata(ctx, c, blob, v, dbPath); err != nil {
		return err
	}
	if !recovered {
		if err = backup.CleanupRecoveryStaging(c.Storage.StateDir); err != nil {
			return fmt.Errorf("clean abandoned recovery staging: %w", err)
		}
	}
	if !c.SMB.ReadOnly {
		if err = r.startManager(ctx, c, blob, dbPath, !v.fresh && !recovered); err != nil {
			return err
		}
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	r.runtime, err = storage.OpenFilesystem(r.metadata, blob, v.format, c.Storage.CacheDir, (*uint64)(c.Storage.CacheSize), r.checkMaintenance)
	if err != nil {
		return err
	}
	// OpenFilesystem has registered the delete callbacks, so the JuiceFS session
	// workers may start. NewSession can start workers even when it returns an
	// error, so record the session first.
	r.session = true
	if err = r.metadata.NewSession(true); err != nil {
		return err
	}
	return r.startSMB(ctx, c.SMB, dbPath)
}

// checkMaintenance allows JuiceFS deletions only while metadata backups protect
// them. Read-only mode has no protection and allows none.
func (r *resources) checkMaintenance() error {
	if r.protection == nil {
		return backup.ErrUnprotected
	}
	return r.protection.Check()
}

// run waits until ctx ends, the metadata backup loop fails or SMB stops.
func (r *resources) run(ctx context.Context, readOnly bool) error {
	var backupFailure <-chan error
	if !readOnly {
		backupFailure = r.startBackup(ctx)
	}
	slog.Info("SMB serving", "address", r.listener.Addr().String(), "read_only", readOnly)
	select {
	case <-ctx.Done():
		return nil
	case err := <-backupFailure:
		select {
		case <-ctx.Done():
			// Shutdown has begun, so the loop's result does not matter.
			return nil
		default:
		}
		if err == nil {
			return errors.New("metadata backup protection stopped unexpectedly")
		}
		return fmt.Errorf("metadata backup protection failed; stopping writable SMB: %w", err)
	case err := <-r.serveDone:
		if err = unexpectedServeError(err); err != nil {
			return fmt.Errorf("SMB serving failed: %w", err)
		}
		if ctx.Err() == nil {
			return errors.New("SMB server stopped unexpectedly")
		}
		return nil
	}
}

// findVolume reads the volume identity from the bucket, or from the newest
// metadata backup when format.json is missing, or asks to create a new volume
// when the bucket is empty.
func findVolume(ctx context.Context, raw object.ObjectStorage, c *config.Resolved, dbPath string) (volume, error) {
	var v volume
	// List the bucket even when a local database exists. Only a complete, empty
	// listing counts as an empty dataset.
	entries, more, _, err := raw.List(ctx, "", "", "", "", 1, false)
	if err != nil {
		return v, errors.New("remote dataset listing failed; refusing initialization")
	}
	if len(entries) == 0 && more {
		return v, errors.New("remote listing is incomplete; refusing initialization")
	}
	remoteEmpty := len(entries) == 0
	format, err := storage.ReadIdentity(ctx, raw)
	identityMissing := errors.Is(err, os.ErrNotExist)
	if err != nil && !identityMissing {
		return v, fmt.Errorf("read remote volume identity: %w", err)
	}
	v.localExists, err = localMetadataExists(dbPath)
	if err != nil {
		return v, err
	}
	switch {
	case identityMissing && !remoteEmpty:
		v.point, format, err = discoverVolume(ctx, raw, c)
	case identityMissing:
		format, err = newVolume(ctx, c, v.localExists)
		v.fresh = true
	case remoteEmpty:
		err = errors.New("remote volume identity contradicts the empty listing")
	}
	if err != nil {
		return v, err
	}
	// The stored format supplies identity and data layout. The bucket and the
	// credentials come from the current configuration.
	format.Storage = "s3"
	format.TrashDays = c.Backup.TrashDays
	format.Bucket, format.AccessKey, format.SecretKey, format.SessionToken = "", "", "", ""
	// Capacity comes from the current configuration, including on recovery.
	// An omitted setting clears any stored limit.
	format.Capacity = uint64(c.Storage.Capacity)
	if !v.fresh && (format.EncryptAlgo != "") != c.Encryption.Enabled {
		return v, errors.New("configured encryption mode differs from the existing dataset")
	}
	v.format = format
	return v, nil
}

// discoverVolume handles a bucket that holds objects but no format.json. It
// takes the volume identity from the newest metadata backup, read with the one
// stored key.
func discoverVolume(ctx context.Context, raw object.ObjectStorage, c *config.Resolved) (*backup.Point, *meta.Format, error) {
	discovered, candidate, err := storage.DiscoverRecoveryVolume(ctx, raw, c.Encryption.Enabled, c.Passphrase)
	if err != nil {
		return nil, nil, fmt.Errorf("discover existing volume without identity: %w", err)
	}
	point, format, err := newestPoint(ctx, discovered, c.Storage.StateDir)
	if err != nil {
		return nil, nil, fmt.Errorf("remote volume identity is missing; refusing initialization: %w", err)
	}
	if format.Name != storage.VolumeName || (format.EncryptAlgo != "") != c.Encryption.Enabled {
		return nil, nil, errors.New("newest metadata backup does not match the configured encryption mode or volume name")
	}
	if candidate != nil && (format.UUID != candidate.UUID || format.EncryptKey != candidate.EncryptKey || format.EncryptAlgo != candidate.EncryptAlgo) {
		return nil, nil, errors.New("newest metadata backup does not match the stored volume key")
	}
	return point, format, nil
}

// newVolume creates the identity of a new volume in an empty bucket after the
// user confirms.
func newVolume(ctx context.Context, c *config.Resolved, localExists bool) (*meta.Format, error) {
	if localExists {
		return nil, errors.New("remote dataset is empty but local metadata exists; refusing initialization")
	}
	if c.SMB.ReadOnly {
		return nil, errors.New("read-only mode cannot initialize an empty dataset")
	}
	if err := confirm("Initialize a genuinely empty S3 dataset? Local metadata and cache are not encrypted on disk."); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return storage.NewFormat(storage.VolumeName, c.Encryption.Enabled, c.Backup.TrashDays)
}

// recoverLocal restores the missing local database from the newest metadata
// backup after the user confirms.
func recoverLocal(ctx context.Context, blob object.ObjectStorage, c *config.Resolved, format *meta.Format, dbPath string) error {
	point, saved, err := newestPoint(ctx, blob, c.Storage.StateDir)
	if err != nil {
		return fmt.Errorf("local metadata is missing; refusing initialization: %w", err)
	}
	if !backup.SameVolume(saved, format) {
		return errors.New("selected recovery point does not match remote volume identity")
	}
	message := fmt.Sprintf("Recover metadata from %s (%s)? Changes after this point may be lost. The old writer MUST be stopped, including on other hosts. Metadata validation is not a full file-content check.", point.Key, point.Time.UTC().Format(time.RFC3339))
	if err = confirm(message); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	// The user confirmed recovery, so remove staging left by an earlier attempt.
	if err = backup.CleanupRecoveryStaging(c.Storage.StateDir); err != nil {
		return fmt.Errorf("clean abandoned recovery staging: %w", err)
	}
	if _, err = backup.Recover(ctx, blob, point.Key, dbPath, c.Storage.CacheDir, format); err != nil {
		return fmt.Errorf("recover selected metadata: %w", err)
	}
	return nil
}

// openMetadata opens the local database. For a new volume it initializes the
// database and publishes the identity and the volume marker.
func (r *resources) openMetadata(ctx context.Context, c *config.Resolved, blob object.ObjectStorage, v volume, dbPath string) error {
	conf := meta.DefaultConf()
	conf.ReadOnly = c.SMB.ReadOnly
	conf.CheckMaintenance = r.checkMaintenance
	var err error
	r.metadata, err = storage.OpenMetadata(dbPath, conf)
	if err != nil {
		return err
	}
	if !v.fresh {
		local, e := r.metadata.Load(true)
		if e != nil {
			return e
		}
		if !backup.SameVolume(local, v.format) {
			return errors.New("local metadata identity or data layout does not match remote dataset")
		}
		if c.SMB.ReadOnly {
			return nil
		}
		return r.metadata.Init(v.format, false)
	}
	if err = r.metadata.Init(v.format, false); err != nil {
		return err
	}
	attr := meta.Attr{Uid: smbfs.UID, Gid: smbfs.GID, Mode: 0o770}
	if errno := r.metadata.SetAttr(meta.WrapContext(ctx), meta.RootInode, meta.SetAttrUID|meta.SetAttrGID|meta.SetAttrMode, 0, &attr); errno != 0 {
		return fmt.Errorf("set root directory ownership: %w", errno)
	}
	if err = storage.PublishIdentity(ctx, r.raw, v.format); err != nil {
		return err
	}
	return storage.PublishMarker(ctx, blob, v.format)
}

// startManager creates the backup manager. SMB starts only once a metadata
// backup of the current database exists: a reused recent one or a new one.
func (r *resources) startManager(ctx context.Context, c *config.Resolved, blob object.ObjectStorage, dbPath string, reuse bool) error {
	manager, err := backup.New(blob, backup.Options{StateDir: c.Storage.StateDir, DatabasePath: dbPath, Interval: c.Backup.Interval, Timeout: c.Backup.Interval, Protection: r.protection})
	if err != nil {
		return err
	}
	r.manager = manager
	if reuse {
		if reused, e := manager.Reuse(ctx); e != nil || reused {
			return e
		}
	}
	if _, err = manager.Backup(ctx); err != nil {
		return fmt.Errorf("initial metadata backup failed; SMB was not started: %w", err)
	}
	return nil
}

func (r *resources) startBackup(ctx context.Context) <-chan error {
	// Manager.Run closes protection on cancellation. Keep it running until
	// accepted SMB deletion work and adapter cleanup have finished.
	backupCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	r.cancelBackup = cancel
	done := make(chan error, 1)
	r.backupDone = done
	go func() {
		done <- r.manager.Run(backupCtx)
		close(done)
	}()
	return done
}

func (r *resources) startSMB(ctx context.Context, c config.SMBConfig, metadataPath string) error {
	var err error
	r.server, r.adapter, err = newSMBServer(r.runtime, c, metadataPath)
	if err != nil {
		return fmt.Errorf("construct SMB server: %w", err)
	}
	r.listener, err = new(net.ListenConfig).Listen(ctx, "tcp", c.Listen)
	if err != nil {
		return fmt.Errorf("listen on configured SMB address (no fallback): %w", err)
	}
	done := make(chan error, 1)
	r.serveDone = done
	go func() {
		done <- r.server.Serve(ctx, r.listener)
		close(done)
	}()
	return nil
}

// close stops SMB first, then the backup loop, then storage. It stops at the
// first failure and keeps the state lock held.
func (r *resources) close(ctx context.Context) error {
	if err := r.stopSMB(ctx); err != nil {
		return err
	}
	if err := r.stopBackup(); err != nil {
		return err
	}
	if err := r.closeStorage(); err != nil {
		return err
	}
	if r.lock != nil {
		return r.lock.Close()
	}
	return nil
}

func (r *resources) stopSMB(ctx context.Context) error {
	if r.server != nil {
		// Shutdown must finish its cleanup even though ctx has ended. The
		// process deadline bounds it and keeps the state lock.
		if err := unexpectedServeError(r.server.Shutdown(context.WithoutCancel(ctx))); err != nil {
			return fmt.Errorf("SMB shutdown failed; state lock retained: %w", err)
		}
	}
	// Serve owns the listener. Close it here too in case startup stopped before
	// Serve registered it. A listener already closed by Shutdown is expected.
	if r.listener != nil {
		if err := r.listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			return fmt.Errorf("listener shutdown failed; state lock retained: %w", err)
		}
	}
	if r.serveDone != nil {
		if err := unexpectedServeError(<-r.serveDone); err != nil {
			return fmt.Errorf("SMB serving failed during shutdown; state lock retained: %w", err)
		}
	}
	if r.adapter != nil {
		if err := r.adapter.Shutdown(); err != nil {
			return fmt.Errorf("handle shutdown failed; state lock retained: %w", err)
		}
	}
	return nil
}

func (r *resources) stopBackup() error {
	if r.cancelBackup != nil {
		r.cancelBackup()
	}
	if r.backupDone != nil {
		// A backup canceled while it retries returns the cancellation joined
		// with the last failed attempt.
		if err := <-r.backupDone; err != nil && !errors.Is(err, context.Canceled) {
			return fmt.Errorf("metadata backup shutdown failed; state lock retained: %w", err)
		}
	}
	if r.manager != nil {
		r.manager.Wait()
	}
	if r.protection != nil {
		r.protection.Close()
	}
	return nil
}

func (r *resources) closeStorage() error {
	if r.runtime != nil {
		if err := r.runtime.Close(); err != nil {
			return fmt.Errorf("JuiceFS filesystem shutdown failed; state lock retained: %w", err)
		}
	}
	if r.metadata != nil {
		if r.session && r.runtime == nil {
			if err := r.metadata.CloseSession(); err != nil {
				return fmt.Errorf("JuiceFS session shutdown failed; state lock retained: %w", err)
			}
		}
		if err := r.metadata.Shutdown(); err != nil {
			return fmt.Errorf("metadata shutdown failed; state lock retained: %w", err)
		}
	}
	if c, ok := r.raw.(io.Closer); ok {
		if err := c.Close(); err != nil {
			return fmt.Errorf("object storage shutdown failed; state lock retained: %w", err)
		}
	}
	return nil
}

// unexpectedServeError removes shutdown signals without hiding joined failures.
// Only a bare context.Canceled is a signal; a wrapped one is a failure.
func unexpectedServeError(err error) error {
	if err == nil {
		return nil
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		var result error
		for _, cause := range joined.Unwrap() {
			result = errors.Join(result, unexpectedServeError(cause))
		}
		return result
	}
	if errors.Is(err, context.Canceled) && errors.Unwrap(err) == nil {
		return nil
	}
	if errors.Is(err, net.ErrClosed) {
		for cause := errors.Unwrap(err); cause != nil; cause = errors.Unwrap(cause) {
			if _, ok := cause.(interface{ Unwrap() []error }); ok {
				return unexpectedServeError(cause)
			}
		}
		return nil
	}
	return err
}

// verifyRemoteMarker accepts a missing volume marker when the newest metadata
// backup matches the volume. It runs before recovered metadata is put in place.
func verifyRemoteMarker(ctx context.Context, blob object.ObjectStorage, format *meta.Format, backupValidated bool, stateDir string) error {
	if err := storage.VerifyMarker(ctx, blob, format); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("verify volume marker: %w", err)
	}
	if backupValidated {
		return nil
	}
	_, saved, err := newestPoint(ctx, blob, stateDir)
	if err != nil {
		return fmt.Errorf("volume marker is missing: %w", err)
	}
	if !backup.SameVolume(saved, format) {
		return errors.New("backup identity does not match remote volume")
	}
	return nil
}

// newestPoint inspects the newest metadata backup. It fails when there is none
// or when the newest one is invalid, and does not try an older one.
func newestPoint(ctx context.Context, blob object.ObjectStorage, stateDir string) (*backup.Point, *meta.Format, error) {
	points, err := backup.List(ctx, blob)
	if err != nil {
		return nil, nil, fmt.Errorf("list metadata backups: %w", err)
	}
	if len(points) == 0 {
		return nil, nil, errors.New("the bucket holds objects but no metadata backup; if the first start of this dataset never finished, empty the bucket prefix " + storage.VolumeName + "/ and the local state directory, then start again")
	}
	format, err := backup.Inspect(ctx, blob, points[0].Key, stateDir)
	if err != nil {
		return nil, nil, fmt.Errorf("newest metadata backup %s is invalid: %w", points[0].Key, err)
	}
	return &points[0], format, nil
}

func localMetadataExists(path string) (bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return false, errors.New("local metadata is not a nonempty regular database; refusing initialization")
	}
	warnPermissions(info, "metadata database", 0o600)
	return true, nil
}
