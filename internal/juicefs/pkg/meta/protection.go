// Copyright 2026 s3-smb contributors. SPDX-License-Identifier: AGPL-3.0-only
package meta

import (
	"errors"
	"net/url"
	"path/filepath"

	"github.com/djosh34/s3-smb/internal/thirdparty/xorm"
)

// NewSQLite is the non-fatal embedding entry point; unlike NewClient it returns
// connection errors to the application's lifecycle owner.
func NewSQLite(path string, conf *Config) (Meta, error) {
	if conf == nil {
		conf = DefaultConf()
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	// This embedding API accepts a filesystem path, not a CLI DSN. Escape
	// literal ?, # and % before native SQL splits the URI's query options.
	// The driver stripped SQLite URI options from the previous bare path.
	// Preserve its ordinary private-cache behavior: activating shared cache
	// here would introduce SQLITE_LOCKED between native readers and writers.
	uri := url.URL{Scheme: "file", Path: abs, RawQuery: "cache=private"}
	return newSQLMeta("sqlite3", uri.String(), conf)
}

// ClearOrphanLocks removes only unregistered SID-zero advisory locks left by
// a previous read-only authority. The caller MUST hold the exclusive application
// state lock, after metadata Load/Init and before NewSession or client access.
// Call for both writable and read-only startup. Registered native sessions keep
// their ordinary native cleanup; no file metadata or object reference is changed.
func ClearOrphanLocks(m Meta) error {
	db, ok := m.(*dbMeta)
	if !ok || db.Name() != "sqlite3" {
		return errors.New("orphan lock cleanup requires native SQLite")
	}
	return db.lockTxn(func(s *xorm.Session) error {
		if _, err := s.Where("sid = ?", uint64(0)).Delete(&flock{}); err != nil {
			return err
		}
		_, err := s.Where("sid = ?", uint64(0)).Delete(&plock{})
		return err
	})
}

func (m *baseMeta) checkMaintenance() error {
	if m.conf.CheckMaintenance != nil {
		return m.conf.CheckMaintenance()
	}
	return nil
}

// Check inside each retry and again immediately before commit. A task queued
// while protected must not retire references after the host has been suspended.
func (m *dbMeta) maintenanceTxn(f func(*xorm.Session) error, inodes ...Ino) error {
	return m.txn(func(s *xorm.Session) error {
		if err := m.checkMaintenance(); err != nil {
			return err
		}
		if err := f(s); err != nil {
			return err
		}
		return m.checkMaintenance()
	}, inodes...)
}
