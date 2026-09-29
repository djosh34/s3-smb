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
	"github.com/djosh34/s3-smb/internal/smbserver"
	"github.com/djosh34/s3-smb/internal/storage"
)

const backupTimeout = 2 * time.Minute

// resources contains actual native resources, in ownership order. No lock is
// released after a failed or stuck close: Main terminates the owning process.
type resources struct {
	lock         *stateLock
	raw          object.ObjectStorage
	metadata     meta.Meta
	session      bool
	runtime      *storage.Runtime
	adapter      *smbfs.FS
	server       *smbserver.Server
	listener     net.Listener
	protection   *backup.Protection
	manager      *backup.Manager
	cancelBackup context.CancelFunc
	backupDone   <-chan error
}

func (r *resources) close() error {
	if r.protection != nil {
		r.protection.Close()
	}
	timer := time.AfterFunc(shutdownTimeout, hardExit)
	defer timer.Stop()
	if r.cancelBackup != nil {
		r.cancelBackup()
	}
	if r.listener != nil {
		_ = r.listener.Close()
	}
	if r.server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := r.server.Shutdown(ctx); err != nil {
			return fmt.Errorf("SMB shutdown failed; state lock retained: %w", err)
		}
	}
	if r.adapter != nil {
		if err := r.adapter.Shutdown(); err != nil {
			return fmt.Errorf("handle shutdown failed; state lock retained: %w", err)
		}
	}
	if r.backupDone != nil {
		<-r.backupDone
	}
	if r.manager != nil {
		r.manager.Wait()
	}
	if r.runtime != nil {
		if err := r.runtime.Close(); err != nil {
			return fmt.Errorf("native filesystem shutdown failed; state lock retained: %w", err)
		}
	}
	if r.metadata != nil {
		if r.session && r.runtime == nil {
			if err := r.metadata.CloseSession(); err != nil {
				return fmt.Errorf("native session shutdown failed; state lock retained: %w", err)
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
			// A failed close is not permission to release live writers' lock.
			// Exit here, keeping r reachable until process termination.
			slog.Error("shutdown failed; terminating with state lock retained", "error", errors.Join(result, closeErr))
			os.Exit(1)
		}
	}()
	var err error
	r.lock, err = lockState(c.Storage.StateDir)
	if err != nil {
		return err
	}
	r.protection, err = backup.NewProtection(c.Backup.Interval, backupTimeout, c.Backup.TrashDays)
	if err != nil {
		return err
	}
	r.raw, err = storage.OpenS3(c)
	if err != nil {
		return err
	}
	// Listing is mandatory even with a local database. An authentication/listing
	// error, or a partial marker/key/backup, never establishes an empty dataset.
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
		// The bootstrap identity is convenient, not the only route to an
		// existing volume. Unlock only an existing unique key, then let a
		// validated native export provide the actual format. Never fall back
		// to a different encryption mode or an older point.
		discovered, candidate, e := storage.DiscoverRecoveryVolume(ctx, r.raw, c.Encryption.Enabled, c.Passphrase)
		if e != nil {
			return fmt.Errorf("discover existing volume without identity: %w", e)
		}
		points, e := backup.List(ctx, discovered)
		if e != nil {
			return e
		}
		if len(points) == 0 {
			return errors.New("remote identity is missing and no native backup establishes a volume; refusing initialization")
		}
		point = &points[0]
		format, e = backup.Inspect(ctx, discovered, point.Key)
		if e != nil {
			return fmt.Errorf("selected backup cannot establish identity; no fallback: %w", e)
		}
		if format.Name != "s3-smb" || (format.EncryptAlgo != "") != c.Encryption.Enabled {
			return errors.New("discovered backup does not match the configured dataset mode or native prefix")
		}
		if candidate != nil && (format.UUID != candidate.UUID || format.EncryptKey != candidate.EncryptKey || format.EncryptAlgo != candidate.EncryptAlgo) {
			return errors.New("discovered backup does not match the existing bootstrap key")
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
		format, err = storage.NewFormat("s3-smb", c.Encryption.Enabled, c.Backup.TrashDays)
		if err != nil {
			return err
		}
		fresh = true
	} else if remoteEmpty {
		return errors.New("remote volume identity contradicts the empty listing")
	}
	if !fresh && (format.EncryptAlgo != "") != c.Encryption.Enabled {
		return errors.New("configured encryption mode differs from the existing dataset")
	}
	blob, err := storage.OpenVolume(ctx, r.raw, format, c.Passphrase, fresh)
	if err != nil {
		return err
	}
	recovered := false
	if !fresh && !localExists {
		points, e := backup.List(ctx, blob)
		if e != nil {
			return fmt.Errorf("list recovery points: %w", e)
		}
		if len(points) == 0 {
			return errors.New("existing remote dataset has no metadata recovery point; refusing initialization")
		}
		point = &points[0]
		saved, e := backup.Inspect(ctx, blob, point.Key)
		if e != nil {
			return fmt.Errorf("selected recovery point is invalid; no older-point fallback: %w", e)
		}
		if !sameVolume(saved, format) {
			return errors.New("selected recovery point does not match remote volume identity")
		}
		message := fmt.Sprintf("Recover metadata from %s (%s)? Changes after this point may be lost. The old writer MUST be stopped, including on other hosts. Metadata validation is not a full file-content check.", point.Key, point.Time.UTC().Format(time.RFC3339))
		if err = confirm(message); err != nil {
			return err
		}
		if err = ctx.Err(); err != nil {
			return err
		}
		// Native SQLite import is synchronous and cannot be interrupted by a
		// context. Terminate the process, retaining the lock, if it gets stuck.
		recoveryTimer := time.AfterFunc(backupTimeout+shutdownTimeout, hardExit)
		_, err = backup.Recover(ctx, blob, point.Key, dbPath, format)
		recoveryTimer.Stop()
		if err != nil {
			return fmt.Errorf("recover selected metadata: %w", err)
		}
		recovered = true
	}
	if !fresh {
		if err = storage.VerifyMarker(ctx, blob, format); err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("verify native volume marker: %w", err)
			}
			// A validated backup supplies identity when the optional native marker is
			// missing. Never synthesize a replacement identity from local SQLite alone.
			if point == nil {
				points, e := backup.List(ctx, blob)
				if e != nil {
					return e
				}
				if len(points) == 0 {
					return errors.New("native marker missing and no validated backup supplies identity")
				}
				saved, e := backup.Inspect(ctx, blob, points[0].Key)
				if e != nil {
					return e
				}
				if !sameVolume(saved, format) {
					return errors.New("backup identity does not match remote volume")
				}
			}
		}
	}
	conf := meta.DefaultConf()
	conf.ReadOnly = c.SMB.ReadOnly
	conf.CheckMaintenance = r.protection.Check
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
			return fmt.Errorf("set native root ownership: %w", errno)
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
		if !sameVolume(local, format) {
			return errors.New("local metadata identity or data layout does not match remote dataset")
		}
		// Current YAML controls retention, not a stale recovery export. It does not
		// supply connection settings: OpenS3 already used the startup TLS snapshot.
		format.TrashDays = c.Backup.TrashDays
		if !c.SMB.ReadOnly {
			if err = r.metadata.Init(format, false); err != nil {
				return err
			}
		}
	}
	var manager *backup.Manager
	if !c.SMB.ReadOnly {
		manager, err = backup.New(r.metadata, blob, backup.Options{StateDir: c.Storage.StateDir, Interval: c.Backup.Interval, Timeout: backupTimeout, Attempts: 3, Protection: r.protection})
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
	// Neither native mutable session jobs nor SMB access exist before protection.
	// NewSession may partially start workers even if it returns an error.
	r.session = true
	if err = r.metadata.NewSession(true); err != nil {
		return err
	}
	var cacheBytes *int64
	if c.Storage.CacheSize != nil {
		v := int64(*c.Storage.CacheSize)
		cacheBytes = &v
	}
	r.runtime, err = storage.OpenFilesystem(r.metadata, blob, format, c.Storage.CacheDir, cacheBytes, r.protection.Check)
	if err != nil {
		return err
	}
	r.adapter, err = smbfs.New(r.runtime.FS, c.SMB.ReadOnly)
	if err != nil {
		return err
	}
	r.server, err = smbserver.New(c.SMB.Share, c.SMB.Username, *c.SMB.Password, r.adapter)
	if err != nil {
		return err
	}
	r.listener, err = net.Listen("tcp", c.SMB.Listen)
	if err != nil {
		return fmt.Errorf("listen on configured SMB address (no fallback): %w", err)
	}
	if !c.Encryption.Enabled {
		slog.Warn("application encryption is disabled; sufficient S3 read access exposes file data and metadata")
	}
	if *c.SMB.Password == "" {
		host, _, _ := net.SplitHostPort(c.SMB.Listen)
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			slog.Warn("named empty-password SMB access is exposed on an explicitly configured non-loopback address")
		}
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- r.server.Serve(r.listener) }()
	var backupFailure <-chan error
	if !c.SMB.ReadOnly {
		backupCtx, cancel := context.WithCancel(ctx)
		r.cancelBackup = cancel
		done := make(chan error, 1)
		r.backupDone = done
		backupFailure = done
		go func() { done <- manager.Run(backupCtx); close(done) }()
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
	case err = <-serveDone:
		if err == nil {
			return errors.New("SMB server stopped unexpectedly")
		}
		return fmt.Errorf("SMB serving failed: %w", err)
	}
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

func sameVolume(a, b *meta.Format) bool {
	return a != nil && b != nil && a.UUID == b.UUID && a.Name == b.Name && a.BlockSize == b.BlockSize && a.Compression == b.Compression && a.Shards == b.Shards && a.HashPrefix == b.HashPrefix && a.EncryptAlgo == b.EncryptAlgo && a.EncryptKey == b.EncryptKey
}
