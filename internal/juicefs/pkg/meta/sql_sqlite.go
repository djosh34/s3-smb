//go:build !nosqlite
// +build !nosqlite

/*
 * JuiceFS, Copyright 2021 Juicedata, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

// Modified for s3-smb, 2026. See docs/vendored.md.

package meta

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/djosh34/s3-smb/internal/thirdparty/xorm"
	"github.com/djosh34/s3-smb/internal/thirdparty/xorm/core"
	"github.com/djosh34/s3-smb/internal/thirdparty/xorm/dialects"
	"github.com/mattn/go-sqlite3"
)

func isSQLiteDuplicateEntryErr(err error) bool {
	if e, ok := err.(sqlite3.Error); ok {
		return e.Code == sqlite3.ErrConstraint
	}
	return false
}

func sqliteFullFSync(conn *sqlite3.SQLiteConn) error {
	// These flags request F_FULLFSYNC on macOS and have no effect elsewhere.
	// The hook runs for every connection, including pool replacements.
	if _, err := conn.Exec("PRAGMA fullfsync=ON; PRAGMA checkpoint_fullfsync=ON", nil); err != nil {
		return fmt.Errorf("enable SQLite full fsync: %w", err)
	}
	return nil
}

func newFullFSyncSQLiteEngine(addr string) (*xorm.Engine, error) {
	dialect, err := dialects.OpenDialect("sqlite3", addr)
	if err != nil {
		return nil, err
	}
	db, err := core.Open("sqlite3_fullfsync", addr)
	if err != nil {
		return nil, err
	}
	// Keep JuiceFS's SQLite engine name and dialect, changing only the driver.
	engine, err := xorm.NewEngineWithDialectAndDB("sqlite3", addr, dialect, db)
	if err != nil {
		return nil, errors.Join(err, db.Close())
	}
	return engine, nil
}

func init() {
	sql.Register("sqlite3_fullfsync", &sqlite3.SQLiteDriver{ConnectHook: sqliteFullFSync})
	engineCreator["sqlite3"] = newFullFSyncSQLiteEngine
	errBusy = sqlite3.ErrBusy
	dupErrorCheckers = append(dupErrorCheckers, isSQLiteDuplicateEntryErr)
	Register("sqlite3", newSQLMeta)
}
