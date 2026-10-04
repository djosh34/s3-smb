// SPDX-License-Identifier: AGPL-3.0-only
package backup

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/djosh34/s3-smb/internal/juicefs/pkg/meta"
	"github.com/djosh34/s3-smb/internal/juicefs/pkg/object"
)

// downloadSnapshot verifies the recorded database SHA-256 before SQLite opens
// the file. The compressed stream is already decrypted by the volume store.
func downloadSnapshot(ctx context.Context, blob object.ObjectStorage, key, path string) (err error) {
	r, err := blob.Get(ctx, key, 0, -1)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, r.Close()) }()
	gz, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, gz.Close()) }()
	digest, ok := strings.CutPrefix(gz.Comment, "sha256:")
	decoded, decodeErr := hex.DecodeString(digest)
	if !ok || decodeErr != nil || len(decoded) != sha256.Size {
		return errors.New("metadata snapshot has no valid SHA-256")
	}
	f, err := os.OpenFile(filepath.Clean(path), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	h := sha256.New()
	if _, err = io.Copy(f, io.TeeReader(gz, h)); err != nil {
		return err
	}
	if hex.EncodeToString(h.Sum(nil)) != digest {
		return errors.New("metadata snapshot SHA-256 mismatch")
	}
	return ctx.Err()
}

func inspectSnapshot(ctx context.Context, path string) (format *meta.Format, err error) {
	db, err := openSnapshotDB(path, "ro")
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	rows, err := db.QueryContext(ctx, "PRAGMA integrity_check")
	if err != nil {
		return nil, fmt.Errorf("snapshot integrity_check: %w", err)
	}
	var results []string
	for rows.Next() {
		var result string
		if err = rows.Scan(&result); err != nil {
			break
		}
		results = append(results, result)
	}
	if err = errors.Join(err, rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	if len(results) != 1 || results[0] != "ok" {
		return nil, errors.New("snapshot integrity_check failed")
	}
	if format, err = readFormat(ctx, db); err != nil {
		return nil, err
	}
	var rootType int
	if err = db.QueryRowContext(ctx, "SELECT type FROM jfs_node WHERE inode=1").Scan(&rootType); err != nil {
		return nil, err
	}
	if format.UUID == "" || format.Name == "" || rootType != meta.TypeDirectory {
		return nil, errors.New("metadata snapshot has no volume identity or root directory")
	}
	if err = format.CheckVersion(); err != nil {
		return nil, err
	}
	return format, ctx.Err()
}

// expireSnapshotSessions marks every session in the staged copy as expired, so
// JuiceFS cleans them, and removes locks of read-only clients. Those use SID
// zero and have no session row.
func expireSnapshotSessions(ctx context.Context, path string) (err error) {
	db, err := openSnapshotDB(path, "rw")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	_, err = db.ExecContext(ctx, "UPDATE jfs_session2 SET expire=0; DELETE FROM jfs_flock WHERE sid=0; DELETE FROM jfs_plock WHERE sid=0")
	return err
}

func snapshotSessionCount(ctx context.Context, path string) (count int, err error) {
	db, err := openSnapshotDB(path, "ro")
	if err != nil {
		return 0, err
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	err = db.QueryRowContext(ctx, "SELECT count(*) FROM jfs_session2").Scan(&count)
	return count, err
}

// CleanStaleSessions reports failures in logs, not its return value. Count the
// remaining rows to detect failure and repeat for its 1000-session batch limit.
func cleanSnapshotSessions(ctx context.Context, m meta.Meta, path string) error {
	previous, err := snapshotSessionCount(ctx, path)
	if err != nil {
		return err
	}
	for previous > 0 {
		if err = ctx.Err(); err != nil {
			return err
		}
		m.CleanStaleSessions(meta.WrapContext(ctx))
		var count int
		if count, err = snapshotSessionCount(ctx, path); err != nil {
			return err
		}
		if count >= previous {
			return errors.New("could not clean restored sessions")
		}
		previous = count
	}
	return checkSnapshotSessions(ctx, path)
}

func checkSnapshotSessions(ctx context.Context, path string) (err error) {
	db, err := openSnapshotDB(path, "ro")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	for _, table := range []string{"jfs_session2", "jfs_flock", "jfs_plock", "jfs_sustained"} {
		var count int
		if err = db.QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			return fmt.Errorf("restored %s table is not empty", table)
		}
	}
	return nil
}
