// Copyright 2026 s3-smb contributors. SPDX-License-Identifier: AGPL-3.0-only
package meta

import "github.com/djosh34/s3-smb/internal/thirdparty/xorm"

// NewSQLite is the non-fatal embedding entry point; unlike NewClient it returns
// connection errors to the application's lifecycle owner.
func NewSQLite(path string, conf *Config) (Meta, error) {
	if conf == nil {
		conf = DefaultConf()
	}
	return newSQLMeta("sqlite3", path, conf)
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
