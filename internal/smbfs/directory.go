package smbfs

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"net/url"
	"path/filepath"
	"time"

	"github.com/djosh34/s3-smb/internal/juicefs/pkg/meta"
	"github.com/djosh34/s3-smb/internal/smb"
)

// Meta only exposes whole-directory reads or stateful, positional cursors. This
// query uses the runtime's SQLite schema and an edge-ID cookie. Removing earlier
// entries cannot shift the cookie, and the parent/ID index bounds each page.
const directoryQuery = `SELECT e.id,e.name,n.inode,n.type,n.length,n.parent,
 n.atime,n.atimensec,n.mtime,n.mtimensec,n.ctime,n.ctimensec,
 birth.value,bits.value,accessed.value,modified.value,changed.value
 FROM jfs_edge e JOIN jfs_node n ON n.inode=e.inode
 LEFT JOIN jfs_xattr birth ON birth.inode=n.inode AND birth.name=?
 LEFT JOIN jfs_xattr bits ON bits.inode=n.inode AND bits.name=?
 LEFT JOIN jfs_xattr accessed ON accessed.inode=n.inode AND accessed.name=?
 LEFT JOIN jfs_xattr modified ON modified.inode=n.inode AND modified.name=?
 LEFT JOIN jfs_xattr changed ON changed.inode=n.inode AND changed.name=?
 WHERE e.parent=? AND e.id>? ORDER BY e.id LIMIT ?`

type directoryEntry struct {
	name   string
	values privateAttrs
	attr   meta.Attr
	inode  smb.Inode
	next   smb.Cookie
}

func directoryDB(path string, readOnly bool) (*sql.DB, error) {
	if !filepath.IsAbs(path) {
		return nil, smb.ErrInvalidParameter
	}
	mode := "rw"
	if readOnly {
		mode = "ro"
	}
	uri := url.URL{Scheme: "file", Path: path}
	query := url.Values{"mode": {mode}, "cache": {"private"}, "_busy_timeout": {"5000"}, "_synchronous": {"FULL"}, "_journal_mode": {"WAL"}}
	uri.RawQuery = query.Encode()
	// Use the runtime hook on every physical connection, including replacements.
	db, err := sql.Open("sqlite3_fullfsync", uri.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	return db, nil
}

func prepareDirectoryPages(path string, readOnly bool) (*sql.DB, error) {
	db, err := directoryDB(path, readOnly)
	if err != nil {
		return nil, err
	}
	if readOnly {
		err = db.PingContext(context.Background())
	} else {
		_, err = db.ExecContext(context.Background(), `CREATE INDEX IF NOT EXISTS IDX_jfs_edge_smbfs_directory_page ON jfs_edge(parent,id)`)
	}
	if err != nil {
		return nil, errors.Join(err, db.Close())
	}
	return db, nil
}

func (s *FS) directoryPage(ctx context.Context, ino smb.Inode, cookie smb.Cookie, limit uint32) (entries []directoryEntry, err error) {
	if uint64(cookie) > math.MaxInt64 {
		return nil, nil
	}
	rows, err := s.directory.QueryContext(ctx, directoryQuery, birthKey, attributesKey, accessedKey, modifiedKey, changedKey, ino, cookie, limit)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	for rows.Next() {
		var entry directoryEntry
		var id int64
		var atime, mtime, ctime int64
		var ansec, mnsec, cnsec int16
		if err = rows.Scan(&id, &entry.name, &entry.inode, &entry.attr.Typ, &entry.attr.Length, &entry.attr.Parent,
			&atime, &ansec, &mtime, &mnsec, &ctime, &cnsec, &entry.values[0], &entry.values[1], &entry.values[2], &entry.values[3], &entry.values[4]); err != nil {
			return nil, err
		}
		if id <= 0 {
			return nil, smb.ErrIO
		}
		entry.next = smb.Cookie(id)
		entry.attr.Atime, entry.attr.Atimensec = databaseTime(atime, ansec)
		entry.attr.Mtime, entry.attr.Mtimensec = databaseTime(mtime, mnsec)
		entry.attr.Ctime, entry.attr.Ctimensec = databaseTime(ctime, cnsec)
		entries = append(entries, entry)
	}
	return entries, rows.Err()
}

func databaseTime(microseconds int64, remainder int16) (int64, uint32) {
	stamp := time.UnixMicro(microseconds).Add(time.Duration(remainder))
	return stamp.Unix(), nanoseconds(stamp)
}
