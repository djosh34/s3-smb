// SPDX-License-Identifier: AGPL-3.0-only
package backup

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"

	"github.com/djosh34/s3-smb/internal/juicefs/pkg/object"
	_ "github.com/mattn/go-sqlite3" // Register the SQLite driver.
)

// openSnapshotDB uses a private cache, never JuiceFS's shared connection cache.
// mode=rw also prevents a missing source database from being created.
func openSnapshotDB(path, mode string) (*sql.DB, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	u := url.URL{Scheme: "file", Path: absolute}
	q := url.Values{"mode": {mode}, "cache": {"private"}, "_busy_timeout": {"5000"}}
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite3", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

func takeSnapshot(ctx context.Context, source, target string) (err error) {
	db, err := openSnapshotDB(source, "ro")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	_, err = db.ExecContext(ctx, "VACUUM INTO ?", target)
	return err
}

func compressSnapshot(source, target string) (err error) {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, in.Close()) }()
	h := sha256.New()
	if _, err = io.Copy(h, in); err != nil {
		return err
	}
	if _, err = in.Seek(0, io.SeekStart); err != nil {
		return err
	}
	out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, out.Close()) }()
	gz, err := gzip.NewWriterLevel(out, gzip.BestSpeed)
	if err != nil {
		return err
	}
	// Keep the database hash in the same published object, inside encryption.
	gz.Comment = "sha256:" + hex.EncodeToString(h.Sum(nil))
	_, err = io.Copy(gz, in)
	if err = errors.Join(err, gz.Close()); err != nil {
		return err
	}
	return out.Sync()
}

// uploadSnapshot keeps conditional publication and full read-back verification.
// A lost upload response burns the name, just as it did before snapshots.
func uploadSnapshot(ctx context.Context, blob object.ObjectStorage, path, key string) (digest string, err error) {
	if _, err = blob.Head(ctx, key); err == nil {
		return "", errors.New("metadata backup name already exists")
	} else if !os.IsNotExist(err) {
		return "", err
	}
	publisher, ok := blob.(interface {
		PutIfAbsent(context.Context, string, io.Reader) error
	})
	if !ok {
		return "", errors.New("metadata store does not support conditional publication")
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	h := sha256.New()
	if _, err = io.Copy(h, f); err != nil {
		return "", err
	}
	digest = hex.EncodeToString(h.Sum(nil))
	if _, err = f.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	if err = publisher.PutIfAbsent(ctx, key, f); err != nil {
		return "", err
	}
	r, err := blob.Get(ctx, key, 0, -1)
	if err != nil {
		return "", err
	}
	h.Reset()
	_, err = io.Copy(h, r)
	if err = errors.Join(err, r.Close()); err != nil {
		return "", err
	}
	if hex.EncodeToString(h.Sum(nil)) != digest {
		return "", errors.New("metadata backup readback mismatch")
	}
	return digest, ctx.Err()
}

func (m *Manager) snapshot(ctx context.Context, key string) (digest string, err error) {
	dir, err := os.MkdirTemp(filepath.Join(m.opts.StateDir, "backup-staging"), "snapshot-")
	if err != nil {
		return "", err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(dir)) }()
	path := filepath.Join(dir, "metadata.db")
	if err = takeSnapshot(ctx, m.opts.DatabasePath, path); err != nil {
		return "", fmt.Errorf("SQLite snapshot: %w", err)
	}
	compressed := filepath.Join(dir, "snapshot.db.gz")
	if err = compressSnapshot(path, compressed); err != nil {
		return "", err
	}
	if err = ctx.Err(); err != nil {
		return "", err
	}
	return uploadSnapshot(ctx, m.blob, compressed, key)
}
