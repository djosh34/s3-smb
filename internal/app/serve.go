// SPDX-License-Identifier: AGPL-3.0-only
package app

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"time"

	"github.com/djosh34/s3-smb/internal/config"
	"github.com/djosh34/s3-smb/internal/engine"
	"github.com/djosh34/s3-smb/internal/smb"
)

type smbServer interface {
	Serve(ctx context.Context, listener net.Listener) error
	Shutdown(ctx context.Context) error
}

// store is the storage engine as serve uses it.
type store interface {
	smb.Storage
	Dead() <-chan struct{}
	Err() error
	Shutdown(ctx context.Context) error
}

// resources holds what serve opened, in the order it was opened. After a failed
// or stuck close the folder lock stays held and the process exits.
type resources struct {
	lock      *stateLock
	store     store
	server    smbServer
	listener  net.Listener
	serveDone <-chan error
}

// serve runs the SMB service until ctx ends or a part of it fails. It calls
// hardExit when shutdown takes longer than shutdownTimeout.
func serve(ctx context.Context, c *config.Resolved, hardExit func()) (result error) {
	var r resources
	defer func() {
		timer := time.AfterFunc(shutdownTimeout, hardExit)
		defer timer.Stop()
		// A failed close may leave a writer running. The folder lock stays held
		// until the process exits, so no second process can start meanwhile.
		if err := r.close(ctx); err != nil {
			result = errors.Join(result, fmt.Errorf("shutdown failed; terminating with the folder lock retained: %w", err))
		}
	}()
	if err := r.open(ctx, c); err != nil {
		return err
	}
	return r.run(ctx, c.SMB.ReadOnly)
}

// open takes the folder lock, opens the engine, which waits for the bucket
// lock and restores the newest copy when needed, and starts SMB. It records
// each resource in r, so close can release it after a failure.
func (r *resources) open(ctx context.Context, c *config.Resolved) error {
	var err error
	r.lock, err = lockState(c.Storage.StateDir)
	if err != nil {
		return err
	}
	// A path-style URL is the default for a custom endpoint, as MinIO needs.
	pathStyle := c.S3.Endpoint != ""
	if c.S3.PathStyle != nil {
		pathStyle = *c.S3.PathStyle
	}
	bucket, err := engine.NewBucket(engine.BucketOptions{
		TLS: c.TLSConfig, Endpoint: c.S3.Endpoint, Region: cmp.Or(c.S3.Region, "us-east-1"), Bucket: c.S3.Bucket,
		AccessKey: c.AccessKey, SecretKey: c.SecretKey, SessionToken: c.SessionToken, PathStyle: pathStyle,
	})
	if err != nil {
		return err
	}
	e, err := engine.Open(ctx, engine.Options{
		Bucket: bucket, Logger: slog.Default(), Dir: c.Storage.StateDir,
		Capacity: uint64(c.Storage.Capacity), ReadOnly: c.SMB.ReadOnly,
	})
	if err != nil {
		return fmt.Errorf("start the storage engine: %w", err)
	}
	r.store = e
	return r.startSMB(ctx, c.SMB)
}

// run waits until ctx ends, the engine stops or SMB stops.
func (r *resources) run(ctx context.Context, readOnly bool) error {
	slog.Info("SMB serving", "address", r.listener.Addr().String(), "read_only", readOnly)
	select {
	case <-ctx.Done():
		return nil
	case <-r.store.Dead():
		return fmt.Errorf("the storage engine stopped: %w", r.store.Err())
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

func (r *resources) startSMB(ctx context.Context, c config.SMBConfig) error {
	var err error
	r.server, err = newSMBServer(r.store, c)
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

// close stops SMB first, then the engine, then releases the folder lock. It
// stops at the first failure and keeps the folder lock held.
func (r *resources) close(ctx context.Context) error {
	if err := r.stopSMB(ctx); err != nil {
		return err
	}
	if r.store != nil {
		// The engine flushes what SMB left dirty. The process deadline bounds it.
		if err := r.store.Shutdown(context.WithoutCancel(ctx)); err != nil {
			return fmt.Errorf("storage engine shutdown failed; folder lock retained: %w", err)
		}
	}
	if r.lock != nil {
		return r.lock.Close()
	}
	return nil
}

func (r *resources) stopSMB(ctx context.Context) error {
	if r.server != nil {
		// Shutdown must finish its cleanup even though ctx has ended. The
		// process deadline bounds it and keeps the folder lock.
		if err := unexpectedServeError(r.server.Shutdown(context.WithoutCancel(ctx))); err != nil {
			return fmt.Errorf("SMB shutdown failed; folder lock retained: %w", err)
		}
	}
	// Serve owns the listener. Close it here too in case startup stopped before
	// Serve registered it. A listener already closed by Shutdown is expected.
	if r.listener != nil {
		if err := r.listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			return fmt.Errorf("listener shutdown failed; folder lock retained: %w", err)
		}
	}
	if r.serveDone != nil {
		if err := unexpectedServeError(<-r.serveDone); err != nil {
			return fmt.Errorf("SMB serving failed during shutdown; folder lock retained: %w", err)
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
