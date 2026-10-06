// SPDX-License-Identifier: AGPL-3.0-only
package engine

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/mattn/go-sqlite3"
)

// Files in the data folder. The database is called db, with SQLite's db-wal
// and db-shm beside it.
const (
	databaseName = "db"
	serverIDName = "server-id"
	copyTemp     = "copy.tmp"
	restoreTemp  = "restore.tmp"
)

// schemaVersion is PRAGMA user_version. A database with another version is
// refused, never migrated.
const schemaVersion = 1

// The files table holds the namespace and each file's size, times (Unix
// nanoseconds) and DOS attributes. The root is row 1 with no parent. A row
// with no parent other than the root is unlinked but still open, and goes
// away at its last close or at the next start. chunks maps a file's chunk
// index to its object, and length is how many of its bytes count. trash
// holds replaced objects with the highest copy sequence captured when they
// were replaced. pending holds early uploads that no row uses yet. copies
// records each copy attempt. state is one row; published is the history of
// the newest copy this database is known to hold, so a start that crashes
// before its start copy lands still keeps the database.
const schema = `
CREATE TABLE files (
	id INTEGER PRIMARY KEY,
	parent INTEGER,
	name TEXT,
	directory INTEGER NOT NULL,
	size INTEGER NOT NULL,
	created INTEGER NOT NULL,
	accessed INTEGER NOT NULL,
	modified INTEGER NOT NULL,
	changed INTEGER NOT NULL,
	attributes INTEGER NOT NULL,
	UNIQUE (parent, name)
);
CREATE INDEX files_by_parent ON files (parent, id);
CREATE TABLE chunks (
	file INTEGER NOT NULL,
	idx INTEGER NOT NULL,
	name TEXT NOT NULL,
	length INTEGER NOT NULL,
	PRIMARY KEY (file, idx)
) WITHOUT ROWID;
CREATE INDEX chunks_by_name ON chunks (name);
CREATE TABLE trash (name TEXT PRIMARY KEY, seq INTEGER NOT NULL) WITHOUT ROWID;
CREATE TABLE pending (name TEXT PRIMARY KEY) WITHOUT ROWID;
CREATE TABLE copies (
	seq INTEGER PRIMARY KEY,
	counter INTEGER NOT NULL,
	history TEXT NOT NULL,
	captured INTEGER NOT NULL,
	landed INTEGER NOT NULL
);
CREATE TABLE state (
	id INTEGER PRIMARY KEY CHECK (id = 1),
	history TEXT NOT NULL,
	published TEXT NOT NULL,
	published_commits INTEGER NOT NULL,
	commits INTEGER NOT NULL,
	volume TEXT NOT NULL
);
`

// fullFSync requests F_FULLFSYNC on macOS for commits and checkpoints. It has
// no effect elsewhere. It runs for every connection, including replacements.
func fullFSync(conn *sqlite3.SQLiteConn) error {
	if _, err := conn.Exec("PRAGMA fullfsync=ON; PRAGMA checkpoint_fullfsync=ON", nil); err != nil {
		return fmt.Errorf("enable SQLite full fsync: %w", err)
	}
	return nil
}

type connector struct {
	driver *sqlite3.SQLiteDriver
	dsn    string
}

func (c connector) Connect(context.Context) (driver.Conn, error) { return c.driver.Open(c.dsn) }

func (c connector) Driver() driver.Driver { return c.driver }

// openSQLite opens a database file with WAL, synchronous=FULL and the full
// fsync hook. mode is rw or ro.
func openSQLite(path, mode string) *sql.DB {
	uri := url.URL{Scheme: "file", Path: path}
	query := url.Values{"mode": {mode}, "cache": {"private"}, "_busy_timeout": {"10000"}}
	if mode != "ro" {
		query["_journal_mode"] = []string{"WAL"}
		query["_synchronous"] = []string{"FULL"}
		query["_txlock"] = []string{"immediate"}
	}
	uri.RawQuery = query.Encode()
	return sql.OpenDB(connector{driver: &sqlite3.SQLiteDriver{ConnectHook: fullFSync}, dsn: uri.String()})
}

// openDatabase opens the database in dir, creating it with a new history when
// it is missing.
func openDatabase(ctx context.Context, dir string) (*sql.DB, error) {
	path := filepath.Join(dir, databaseName)
	fresh, err := missingDatabase(path)
	if err != nil {
		return nil, err
	}
	if fresh {
		// A WAL beside a missing or empty database is from a first start that
		// never served. SQLite gives db-wal and db-shm the mode of the database.
		for _, suffix := range []string{"-wal", "-shm"} {
			if err = os.Remove(path + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
				return nil, err
			}
		}
		if err = writeSynced(path, nil); err != nil {
			return nil, err
		}
	}
	db := openSQLite(path, "rw")
	if fresh {
		err = createSchema(ctx, db)
	} else {
		err = checkVersion(ctx, db)
	}
	if err != nil {
		return nil, errors.Join(err, db.Close())
	}
	return db, nil
}

// missingDatabase reports a database file that is absent or empty. An empty
// one is left by a crash during the first start.
func missingDatabase(path string) (bool, error) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return info.Size() == 0, nil
}

func checkVersion(ctx context.Context, db *sql.DB) error {
	var version int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version != schemaVersion {
		return fmt.Errorf("database has schema version %d, want %d", version, schemaVersion)
	}
	return nil
}

func createSchema(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	history, err := randomID()
	if err == nil {
		var volume string
		volume, err = randomID()
		if err == nil {
			_, err = tx.ExecContext(ctx, schema+fmt.Sprintf("PRAGMA user_version=%d;", schemaVersion))
		}
		if err == nil {
			_, err = tx.ExecContext(ctx, `INSERT INTO state (id, history, published, published_commits, commits, volume) VALUES (1, ?, '', 0, 0, ?)`, history, volume)
		}
	}
	if err == nil {
		now := timeValue(wallClock())
		_, err = tx.ExecContext(ctx, `INSERT INTO files (id, parent, name, directory, size, created, accessed, modified, changed, attributes)
			VALUES (?, NULL, '', 1, 0, ?, ?, ?, ?, ?)`, rootInode, now, now, now, now, attributeDirectory)
	}
	if err != nil {
		return errors.Join(err, tx.Rollback())
	}
	return tx.Commit()
}

// checkFile runs checkDatabase on the database file at path.
func checkFile(ctx context.Context, path string) error {
	db := openSQLite(path, "ro")
	return errors.Join(checkDatabase(ctx, db), db.Close())
}

// checkDatabase returns nil when PRAGMA quick_check passes and the schema
// version is ours. A file that is not a database fails too.
func checkDatabase(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, "PRAGMA quick_check")
	if err != nil {
		return err
	}
	var lines []string
	for rows.Next() {
		var line string
		if err = rows.Scan(&line); err != nil {
			break
		}
		lines = append(lines, line)
	}
	if err = errors.Join(err, rows.Err(), rows.Close()); err != nil {
		return err
	}
	if len(lines) != 1 || lines[0] != "ok" {
		return fmt.Errorf("quick_check failed: %s", strings.Join(lines, "; "))
	}
	return checkVersion(ctx, db)
}

// stateRow is the one row of the state table.
type stateRow struct {
	history   string
	published string
	volume    string
	commits   int64
	// publishedCommits is the commit counter of that copy.
	publishedCommits int64
}

func readState(ctx context.Context, db *sql.DB) (stateRow, error) {
	var s stateRow
	err := db.QueryRowContext(ctx, `SELECT history, published, published_commits, commits, volume FROM state WHERE id = 1`).Scan(
		&s.history, &s.published, &s.publishedCommits, &s.commits, &s.volume)
	return s, err
}

// readFileState reads the state row of a database file, such as a copy.
func readFileState(ctx context.Context, path string) (stateRow, error) {
	db := openSQLite(path, "ro")
	s, err := readState(ctx, db)
	return s, errors.Join(err, db.Close())
}
