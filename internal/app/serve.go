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
	Serve(context.Context, net.Listener) error
	Shutdown(context.Context) error
}

type smbAdapter interface {
	Shutdown() error
}

// resources holds what serve opened, in the order it was opened. After a failed
// or stuck close the state lock stays held and the process exits.
type resources struct {
	lock         *stateLock
	raw          object.ObjectStorage
	metadata     meta.Meta
	session      bool
	runtime      *storage.Runtime
	adapter      smbAdapter
	server       smbServer
	listener     net.Listener
	serveDone    <-chan error
	protection   *backup.Protection
	manager      *backup.Manager
	cancelBackup context.CancelFunc
	backupDone   <-chan error
}

func (r *resources) close() error {
	timer := time.AfterFunc(shutdownTimeout, hardExit)
	defer timer.Stop()
	if r.server != nil {
		// A caller deadline can return before the server finishes cleanup.
		// Wait for completion; the process deadline retains the state lock.
		if err := unexpectedServeError(r.server.Shutdown(context.Background())); err != nil {
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
	if r.cancelBackup != nil {
		r.cancelBackup()
	}
	if r.backupDone != nil {
		if err := <-r.backupDone; err != nil && err != context.Canceled {
			return fmt.Errorf("metadata backup shutdown failed; state lock retained: %w", err)
		}
	}
	if r.manager != nil {
		r.manager.Wait()
	}
	if r.protection != nil {
		r.protection.Close()
	}
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
	if r.lock != nil {
		return r.lock.Close()
	}
	return nil
}

func serve(ctx context.Context, c *config.Resolved) (result error) {
	var r resources
	defer func() {
		if closeErr := r.close(); closeErr != nil {
			// A failed close may leave a writer running. Exit with the state lock
			// held, so no second process can start while that writer is alive.
			exitFailure("shutdown failed; terminating with state lock retained", errors.Join(result, closeErr))
		}
	}()
	var err error
	r.lock, err = lockState(c.Storage.StateDir)
	if err != nil {
		return err
	}
	checkMaintenance := func() error { return backup.ErrUnprotected }
	if !c.SMB.ReadOnly {
		// A metadata backup may take as long as the backup interval.
		r.protection, err = backup.NewProtection(c.Backup.Interval, c.Backup.Interval, c.Backup.TrashDays)
		if err != nil {
			return err
		}
		checkMaintenance = r.protection.Check
	}
	r.raw, err = storage.OpenS3(c)
	if err != nil {
		return err
	}
	// List the bucket even when a local database exists. Only a complete, empty
	// listing counts as an empty dataset.
	entries, more, _, err := r.raw.List(ctx, "", "", "", "", 1, false)
	if err != nil {
		return errors.New("remote dataset listing failed; refusing initialization")
	}
	if len(entries) == 0 && more {
		return errors.New("remote listing is incomplete; refusing initialization")
	}
	remoteEmpty := len(entries) == 0
	format, err := storage.ReadIdentity(ctx, r.raw)
	identityMissing := errors.Is(err, os.ErrNotExist)
	if err != nil && !identityMissing {
		return fmt.Errorf("read remote volume identity: %w", err)
	}
	dbPath := filepath.Join(c.Storage.StateDir, "metadata.db")
	localExists, err := localMetadataExists(dbPath)
	if err != nil {
		return err
	}
	fresh := false
	var point *backup.Point
	if identityMissing && !remoteEmpty {
		// format.json is missing but the bucket is not empty. Take the volume
		// identity from the newest metadata backup, read with the one stored key.
		discovered, candidate, e := storage.DiscoverRecoveryVolume(ctx, r.raw, c.Encryption.Enabled, c.Passphrase)
		if e != nil {
			return fmt.Errorf("discover existing volume without identity: %w", e)
		}
		point, format, e = newestPoint(ctx, discovered, c.Storage.StateDir)
		if e != nil {
			return fmt.Errorf("remote volume identity is missing; refusing initialization: %w", e)
		}
		if format.Name != storage.VolumeName || (format.EncryptAlgo != "") != c.Encryption.Enabled {
			return errors.New("newest metadata backup does not match the configured encryption mode or volume name")
		}
		if candidate != nil && (format.UUID != candidate.UUID || format.EncryptKey != candidate.EncryptKey || format.EncryptAlgo != candidate.EncryptAlgo) {
			return errors.New("newest metadata backup does not match the stored volume key")
		}
	} else if identityMissing {
		if localExists {
			return errors.New("remote dataset is empty but local metadata exists; refusing initialization")
		}
		if c.SMB.ReadOnly {
			return errors.New("read-only mode cannot initialize an empty dataset")
		}
		if err = confirm("Initialize a genuinely empty S3 dataset? Local metadata and cache are not encrypted on disk."); err != nil {
			return err
		}
		if err = ctx.Err(); err != nil {
			return err
		}
		format, err = storage.NewFormat(storage.VolumeName, c.Encryption.Enabled, c.Backup.TrashDays)
		if err != nil {
			return err
		}
		fresh = true
	} else if remoteEmpty {
		return errors.New("remote volume identity contradicts the empty listing")
	}
	// The stored format supplies identity and data layout. The bucket and the
	// credentials come from the current configuration.
	format.Storage = "s3"
	format.TrashDays = c.Backup.TrashDays
	format.Bucket, format.AccessKey, format.SecretKey, format.SessionToken = "", "", "", ""
	// Capacity comes from the current configuration, including on recovery.
	// An omitted setting clears any stored limit.
	format.Capacity = uint64(c.Storage.Capacity)
	if !fresh && (format.EncryptAlgo != "") != c.Encryption.Enabled {
		return errors.New("configured encryption mode differs from the existing dataset")
	}
	blob, err := storage.OpenVolume(ctx, r.raw, format, c.Passphrase, fresh)
	if err != nil {
		return err
	}
	if !fresh {
		if err = verifyRemoteMarker(ctx, blob, format, point != nil, c.Storage.StateDir); err != nil {
			return err
		}
	}
	recovered := false
	if !fresh && !localExists {
		var saved *meta.Format
		point, saved, err = newestPoint(ctx, blob, c.Storage.StateDir)
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
		// The user confirmed recovery, so remove staging left by an earlier
		// attempt.
		if err = backup.CleanupRecoveryStaging(c.Storage.StateDir); err != nil {
			return fmt.Errorf("clean abandoned recovery staging: %w", err)
		}
		if _, err = backup.Recover(ctx, blob, point.Key, dbPath, c.Storage.CacheDir, format); err != nil {
			return fmt.Errorf("recover selected metadata: %w", err)
		}
		recovered = true
	}
	conf := meta.DefaultConf()
	conf.ReadOnly = c.SMB.ReadOnly
	conf.CheckMaintenance = checkMaintenance
	r.metadata, err = storage.OpenMetadata(dbPath, conf)
	if err != nil {
		return err
	}
	if fresh {
		if err = r.metadata.Init(format, false); err != nil {
			return err
		}
		attr := meta.Attr{Uid: smbfs.UID, Gid: smbfs.GID, Mode: 0770}
		if errno := r.metadata.SetAttr(meta.Background(), meta.RootInode, meta.SetAttrUID|meta.SetAttrGID|meta.SetAttrMode, 0, &attr); errno != 0 {
			return fmt.Errorf("set root directory ownership: %w", errno)
		}
		if err = storage.PublishIdentity(ctx, r.raw, format); err != nil {
			return err
		}
		if err = storage.PublishMarker(ctx, blob, format); err != nil {
			return err
		}
	} else {
		local, e := r.metadata.Load(true)
		if e != nil {
			return e
		}
		if !backup.SameVolume(local, format) {
			return errors.New("local metadata identity or data layout does not match remote dataset")
		}
		if !c.SMB.ReadOnly {
			if err = r.metadata.Init(format, false); err != nil {
				return err
			}
		}
	}
	if !recovered {
		if err = backup.CleanupRecoveryStaging(c.Storage.StateDir); err != nil {
			return fmt.Errorf("clean abandoned recovery staging: %w", err)
		}
	}
	if err = r.prepareSMBMetadata(); err != nil {
		return err
	}
	var manager *backup.Manager
	if !c.SMB.ReadOnly {
		manager, err = backup.New(r.metadata, blob, backup.Options{StateDir: c.Storage.StateDir, DatabasePath: dbPath, Interval: c.Backup.Interval, Timeout: c.Backup.Interval, Attempts: 3, Protection: r.protection})
		if err != nil {
			return err
		}
		r.manager = manager
		reused := false
		if !fresh && !recovered {
			reused, err = manager.Reuse(ctx)
			if err != nil {
				return err
			}
		}
		if !reused {
			if _, err = manager.Backup(ctx); err != nil {
				return fmt.Errorf("initial metadata backup failed; SMB was not started: %w", err)
			}
		}
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	var cacheBytes *int64
	if c.Storage.CacheSize != nil {
		v := int64(*c.Storage.CacheSize)
		cacheBytes = &v
	}
	r.runtime, err = storage.OpenFilesystem(r.metadata, blob, format, c.Storage.CacheDir, cacheBytes, checkMaintenance)
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
	if err = r.startSMB(ctx, c.SMB, dbPath); err != nil {
		return err
	}
	var backupFailure <-chan error
	if !c.SMB.ReadOnly {
		backupFailure = r.startBackup(ctx)
	}
	slog.Info("SMB serving", "address", r.listener.Addr().String(), "read_only", c.SMB.ReadOnly)
	select {
	case <-ctx.Done():
		return nil
	case err = <-backupFailure:
		if ctx.Err() != nil {
			return nil
		}
		if err == nil {
			return errors.New("metadata backup protection stopped unexpectedly")
		}
		return fmt.Errorf("metadata backup protection failed; stopping writable SMB: %w", err)
	case err = <-r.serveDone:
		err = unexpectedServeError(err)
		if err == nil && ctx.Err() != nil {
			return nil
		}
		if err == nil {
			return errors.New("SMB server stopped unexpectedly")
		}
		return fmt.Errorf("SMB serving failed: %w", err)
	}
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

// unexpectedServeError removes shutdown signals without hiding joined failures.
func unexpectedServeError(err error) error {
	if err == nil || err == context.Canceled {
		return nil
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		var result error
		for _, cause := range joined.Unwrap() {
			result = errors.Join(result, unexpectedServeError(cause))
		}
		return result
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

func (r *resources) startSMB(ctx context.Context, c config.SMBConfig, metadataPath string) error {
	var err error
	r.server, r.adapter, err = newSMBServer(r.runtime, c, metadataPath)
	if err != nil {
		return fmt.Errorf("construct SMB server: %w", err)
	}
	r.listener, err = net.Listen("tcp", c.Listen)
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
	warnPermissions(info, "metadata database", 0600)
	return true, nil
}
