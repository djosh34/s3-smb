// SPDX-License-Identifier: AGPL-3.0-only
package engine

import (
	"context"
	"database/sql"
	"errors"
	"hash/fnv"
	"math"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/djosh34/s3-smb/internal/smb"
)

const (
	rootInode          = 1
	attributeDirectory = 0x10
	attributeNormal    = 0x80
	// maxDepth bounds a walk up the parents, which only a damaged database
	// could make endless.
	maxDepth = 4096
)

// row is one row of the files table.
type row struct {
	name       sql.NullString
	parent     sql.Null[smb.Inode]
	id         smb.Inode
	size       uint64
	created    int64
	accessed   int64
	modified   int64
	changed    int64
	attributes uint32
	directory  bool
}

const rowColumns = `id, parent, name, directory, size, created, accessed, modified, changed, attributes`

type scanner interface{ Scan(dest ...any) error }

func scanRow(s scanner) (row, error) {
	var r row
	err := s.Scan(&r.id, &r.parent, &r.name, &r.directory, &r.size, &r.created, &r.accessed, &r.modified, &r.changed, &r.attributes)
	return r, err
}

type querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// fileRow reads a file by ID. A missing row is ErrNameNotFound.
func fileRow(ctx context.Context, q querier, id smb.Inode) (row, error) {
	if id == 0 || id > math.MaxInt64 {
		return row{}, smb.ErrInvalidParameter
	}
	r, err := scanRow(q.QueryRowContext(ctx, `SELECT `+rowColumns+` FROM files WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return row{}, smb.ErrNameNotFound
	}
	return r, storageError(err)
}

// childRow reads the entry name in parent. ok is false when it is missing.
func childRow(ctx context.Context, q querier, parent smb.Inode, name string) (r row, ok bool, err error) {
	r, err = scanRow(q.QueryRowContext(ctx, `SELECT `+rowColumns+` FROM files WHERE parent = ? AND name = ?`, parent, name))
	if errors.Is(err, sql.ErrNoRows) {
		return row{}, false, nil
	}
	return r, err == nil, storageError(err)
}

func timeValue(t time.Time) int64 {
	// UnixNano covers the years 1678 to 2262. Clamp anything outside.
	switch {
	case t.Before(time.Unix(0, math.MinInt64)):
		return math.MinInt64
	case t.After(time.Unix(0, math.MaxInt64)):
		return math.MaxInt64
	}
	return t.UnixNano()
}

func timeOf(value int64) time.Time { return time.Unix(0, value).UTC() }

func wallClock() time.Time { return time.Now().UTC() }

func allocation(size uint64) uint64 { return (size + 4095) / 4096 * 4096 }

func volumeIdentity(volume string) (uint64, error) {
	hash := fnv.New64a()
	if _, err := hash.Write([]byte(volume)); err != nil {
		return 0, err
	}
	return hash.Sum64(), nil
}

// attr builds the attributes of r, with the live size and write times of an
// inode that has them.
func attr(r row, st *inode) smb.Attr {
	a := smb.Attr{
		Inode: r.id, Size: r.size, Attributes: r.attributes,
		Created: timeOf(r.created), Accessed: timeOf(r.accessed), Modified: timeOf(r.modified), Changed: timeOf(r.changed),
	}
	if r.directory {
		a.Kind = smb.KindDirectory
		a.Size = 0
	} else if st != nil {
		live := st.snapshot()
		if live.loaded {
			a.Size = live.size
		}
		if live.timesDirty {
			a.Modified, a.Changed = live.modified, live.changed
		}
	}
	a.AllocationSize = allocation(a.Size)
	return a
}

func validBase(base string) bool {
	return base != "" && base != "." && base != ".." && len(base) <= 255 && utf8.ValidString(base) && !strings.ContainsAny(base, "\x00:/\\")
}

// parsePath splits a share-relative path. Named streams are not supported;
// name::$DATA names the file itself.
func parsePath(p string) ([]string, error) {
	if !utf8.ValidString(p) || strings.ContainsRune(p, 0) {
		return nil, smb.ErrInvalidName
	}
	p = strings.ReplaceAll(p, "\\", "/")
	if p == "" {
		return nil, nil
	}
	if strings.HasPrefix(p, "/") {
		return nil, smb.ErrInvalidName
	}
	parts := strings.Split(p, "/")
	last := strings.Split(parts[len(parts)-1], ":")
	switch {
	case len(last) == 1:
	case len(last) == 3 && last[1] == "" && strings.EqualFold(last[2], "$DATA"):
	case len(last) <= 3:
		return nil, smb.ErrNotSupported
	default:
		return nil, smb.ErrInvalidName
	}
	parts[len(parts)-1] = last[0]
	for _, part := range parts {
		if !validBase(part) {
			return nil, smb.ErrInvalidName
		}
	}
	return parts, nil
}

func checkName(name smb.Name) error {
	if name.Stream != "" {
		return smb.ErrNotSupported
	}
	if !validBase(name.Base) {
		return smb.ErrInvalidName
	}
	if name.Parent == 0 {
		return smb.ErrInvalidParameter
	}
	return nil
}

// Lookup resolves a path. A missing final name returns Exists=false.
func (e *Engine) Lookup(ctx context.Context, p string) (smb.Resolved, error) {
	parts, err := parsePath(p)
	if err != nil {
		return smb.Resolved{}, err
	}
	parent := smb.Inode(rootInode)
	if len(parts) == 0 {
		key := smb.ObjectKey{Inode: parent}
		a, attrErr := e.GetAttr(ctx, key)
		return smb.Resolved{Object: key, Attr: a, Exists: attrErr == nil}, attrErr
	}
	for _, base := range parts[:len(parts)-1] {
		r, ok, lookupErr := childRow(ctx, e.db, parent, base)
		switch {
		case lookupErr != nil:
			return smb.Resolved{}, lookupErr
		case !ok:
			return smb.Resolved{}, smb.ErrPathNotFound
		case !r.directory:
			return smb.Resolved{}, smb.ErrNotDirectory
		}
		parent = r.id
	}
	name := smb.Name{Parent: parent, Base: parts[len(parts)-1]}
	r, ok, err := childRow(ctx, e.db, parent, name.Base)
	if err != nil || !ok {
		return smb.Resolved{Name: name}, err
	}
	st, unpin := e.pin(r.id)
	defer unpin()
	return smb.Resolved{Name: name, Object: smb.ObjectKey{Inode: r.id}, Attr: attr(r, st), Exists: true}, nil
}

// Create makes a new file or directory. An existing name is ErrNameCollision.
func (e *Engine) Create(ctx context.Context, name smb.Name, kind smb.Kind) (smb.Resolved, error) {
	if e.readOnly {
		return smb.Resolved{}, smb.ErrReadOnly
	}
	if err := checkName(name); err != nil {
		return smb.Resolved{}, err
	}
	if kind != smb.KindFile && kind != smb.KindDirectory {
		return smb.Resolved{}, smb.ErrInvalidParameter
	}
	now := timeValue(wallClock())
	attributes, directory := attributeNormal, kind == smb.KindDirectory
	if directory {
		attributes = attributeDirectory
	}
	var id smb.Inode
	err := e.commit(ctx, func(tx *sql.Tx) error {
		if err := checkParent(ctx, tx, name.Parent); err != nil {
			return err
		}
		_, exists, err := childRow(ctx, tx, name.Parent, name.Base)
		if err != nil {
			return err
		}
		if exists {
			return smb.ErrNameCollision
		}
		err = tx.QueryRowContext(ctx, `INSERT INTO files (parent, name, directory, size, created, accessed, modified, changed, attributes)
			VALUES (?, ?, ?, 0, ?, ?, ?, ?, ?) RETURNING id`, name.Parent, name.Base, directory, now, now, now, now, attributes).Scan(&id)
		if err != nil {
			return err
		}
		return touch(ctx, tx, now, name.Parent)
	})
	if err != nil {
		return smb.Resolved{}, err
	}
	key := smb.ObjectKey{Inode: id}
	a, err := e.GetAttr(ctx, key)
	return smb.Resolved{Name: name, Object: key, Attr: a, Exists: err == nil}, err
}

// checkParent requires a linked directory.
func checkParent(ctx context.Context, tx *sql.Tx, parent smb.Inode) error {
	r, err := fileRow(ctx, tx, parent)
	switch {
	case err != nil:
		return err
	case !r.directory:
		return smb.ErrNotDirectory
	case !r.parent.Valid && r.id != rootInode:
		return smb.ErrNameNotFound
	}
	return nil
}

// touch sets the modified and changed times of directories after an entry
// changed.
func touch(ctx context.Context, tx *sql.Tx, now int64, dirs ...smb.Inode) error {
	for _, dir := range slices.Compact(dirs) {
		if _, err := tx.ExecContext(ctx, `UPDATE files SET modified = ?, changed = ? WHERE id = ?`, now, now, dir); err != nil {
			return err
		}
	}
	return nil
}

// ReadDir returns up to limit entries after cookie, ordered by ID. A cookie
// is the ID of the last entry returned, so removals do not move it.
func (e *Engine) ReadDir(ctx context.Context, ino smb.Inode, cookie smb.Cookie, limit uint32) (entries []smb.DirEntry, err error) {
	if limit == 0 {
		return nil, smb.ErrInvalidParameter
	}
	dir, err := fileRow(ctx, e.db, ino)
	if err != nil {
		return nil, err
	}
	if !dir.directory {
		return nil, smb.ErrNotDirectory
	}
	if uint64(cookie) >= math.MaxInt64 {
		return nil, nil
	}
	rows, err := e.db.QueryContext(ctx, `SELECT `+rowColumns+` FROM files WHERE parent = ? AND id > ? ORDER BY id LIMIT ?`,
		ino, cookie, limit)
	if err != nil {
		return nil, storageError(err)
	}
	defer func() { err = errors.Join(err, storageError(rows.Close())) }()
	for rows.Next() {
		r, scanErr := scanRow(rows)
		if scanErr != nil {
			return nil, storageError(scanErr)
		}
		st, unpin := e.pin(r.id)
		entries = append(entries, smb.DirEntry{Name: r.name.String, Attr: attr(r, st), Next: smb.Cookie(r.id)})
		unpin()
	}
	return entries, storageError(rows.Err())
}

// Remove deletes name if it is still expect. Its chunks go to the trash in
// the same commit. An open file is only unlinked; its last close drops it.
func (e *Engine) Remove(ctx context.Context, name smb.Name, expect smb.Inode) error {
	if e.readOnly {
		return smb.ErrReadOnly
	}
	if err := checkName(name); err != nil {
		return err
	}
	if expect == 0 {
		return smb.ErrInvalidParameter
	}
	st, release := e.acquire(expect)
	defer release()
	var drop bool
	err := e.commit(ctx, func(tx *sql.Tx) error {
		r, exists, err := childRow(ctx, tx, name.Parent, name.Base)
		if err != nil {
			return err
		}
		if !exists || r.id != expect {
			return smb.ErrIdentityChanged
		}
		if drop, err = e.unlink(ctx, tx, r, st); err != nil {
			return err
		}
		return touch(ctx, tx, timeValue(wallClock()), name.Parent)
	})
	if err != nil {
		return err
	}
	e.settleUnlink(st, drop)
	return nil
}

// unlink removes r from the namespace inside tx. It drops the file at once
// when nothing has it open, and reports whether it did.
func (e *Engine) unlink(ctx context.Context, tx *sql.Tx, r row, st *inode) (bool, error) {
	if r.directory {
		var children bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM files WHERE parent = ?)`, r.id).Scan(&children); err != nil {
			return false, err
		}
		if children {
			return false, smb.ErrDirectoryNotEmpty
		}
	}
	if st.refs > 0 {
		_, err := tx.ExecContext(ctx, `UPDATE files SET parent = NULL, name = NULL WHERE id = ?`, r.id)
		return false, err
	}
	return true, e.dropFile(ctx, tx, r.id, st)
}

// settleUnlink updates memory after a committed unlink.
func (e *Engine) settleUnlink(st *inode, dropped bool) {
	if dropped {
		e.discard(st)
	} else {
		st.unlinked = true
	}
}

// dropFile deletes a file row inside tx and sends its chunks and early
// uploads to the trash. Call discard after the commit.
func (e *Engine) dropFile(ctx context.Context, tx *sql.Tx, id smb.Inode, st *inode) error {
	statements := []statement{
		{`INSERT INTO trash (name, seq) SELECT name, ? FROM chunks WHERE file = ?`, []any{e.captureSeq, id}},
		{`DELETE FROM chunks WHERE file = ?`, []any{id}},
		{`DELETE FROM files WHERE id = ?`, []any{id}},
	}
	for _, early := range st.early {
		statements = append(statements, trashPending(early.name, e.captureSeq)...)
	}
	return execAll(ctx, tx, statements)
}

func trashPending(name string, seq int64) []statement {
	return []statement{
		{`DELETE FROM pending WHERE name = ?`, []any{name}},
		{`INSERT INTO trash (name, seq) VALUES (?, ?)`, []any{name, seq}},
	}
}

// Rename moves exactly the expected identities in one commit. Chunk IDs stay.
// A replaced destination is removed as Remove would.
func (e *Engine) Rename(ctx context.Context, request smb.RenameRequest) error {
	if request.Source.Stream != "" || request.Destination.Stream != "" {
		return smb.ErrNotSupported
	}
	if e.readOnly {
		return smb.ErrReadOnly
	}
	if request.SourceInode == 0 {
		return smb.ErrInvalidParameter
	}
	if err := errors.Join(checkName(request.Source), checkName(request.Destination)); err != nil {
		return err
	}
	var target *inode
	if request.DestinationInode != 0 && request.DestinationInode != request.SourceInode {
		st, release := e.acquire(request.DestinationInode)
		defer release()
		target = st
	}
	var drop bool
	err := e.commit(ctx, func(tx *sql.Tx) (err error) {
		drop, err = e.rename(ctx, tx, request, target)
		return err
	})
	if err != nil {
		return err
	}
	if target != nil {
		e.settleUnlink(target, drop)
	}
	return nil
}

func (e *Engine) rename(ctx context.Context, tx *sql.Tx, request smb.RenameRequest, target *inode) (bool, error) {
	source, exists, err := childRow(ctx, tx, request.Source.Parent, request.Source.Base)
	if err != nil {
		return false, err
	}
	if !exists || source.id != request.SourceInode {
		return false, smb.ErrIdentityChanged
	}
	if err = checkParent(ctx, tx, request.Destination.Parent); err != nil {
		return false, err
	}
	if source.directory {
		if err = checkAncestry(ctx, tx, request.SourceInode, request.Destination.Parent); err != nil {
			return false, err
		}
	}
	destination, exists, err := childRow(ctx, tx, request.Destination.Parent, request.Destination.Base)
	switch {
	case err != nil:
		return false, err
	case exists != (request.DestinationInode != 0) || exists && destination.id != request.DestinationInode:
		return false, smb.ErrIdentityChanged
	case exists && !request.Replace:
		return false, smb.ErrNameCollision
	}
	var drop bool
	if exists && destination.id != source.id {
		switch {
		case destination.directory && !source.directory:
			return false, smb.ErrIsDirectory
		case !destination.directory && source.directory:
			return false, smb.ErrNotDirectory
		}
		if drop, err = e.unlink(ctx, tx, destination, target); err != nil {
			return false, err
		}
	}
	now := timeValue(wallClock())
	err = execAll(ctx, tx, []statement{
		{`UPDATE files SET parent = ?, name = ?, changed = ? WHERE id = ?`, []any{request.Destination.Parent, request.Destination.Base, now, source.id}},
	})
	if err == nil {
		err = touch(ctx, tx, now, request.Source.Parent, request.Destination.Parent)
	}
	return drop, err
}

// checkAncestry refuses to move a directory into itself or below itself.
func checkAncestry(ctx context.Context, tx *sql.Tx, source, parent smb.Inode) error {
	for depth := 0; parent != rootInode; depth++ {
		if parent == source {
			return smb.ErrInvalidParameter
		}
		r, err := fileRow(ctx, tx, parent)
		if err != nil {
			return err
		}
		if !r.parent.Valid || depth > maxDepth {
			return smb.ErrIO
		}
		parent = r.parent.V
	}
	return nil
}

// PathOf walks up the parents. An unlinked file is not found.
func (e *Engine) PathOf(ctx context.Context, ino smb.Inode) (string, error) {
	if ino == 0 {
		return "", smb.ErrInvalidParameter
	}
	var names []string
	for depth := 0; ino != rootInode; depth++ {
		r, err := fileRow(ctx, e.db, ino)
		if err != nil {
			return "", err
		}
		if !r.parent.Valid {
			return "", smb.ErrNameNotFound
		}
		if depth > maxDepth {
			return "", smb.ErrIO
		}
		names = append(names, r.name.String)
		ino = r.parent.V
	}
	slices.Reverse(names)
	return strings.Join(names, "/"), nil
}

// Streams lists nothing: named streams are not supported.
func (e *Engine) Streams(ctx context.Context, ino smb.Inode) ([]smb.StreamInfo, error) {
	_, err := fileRow(ctx, e.db, ino)
	return nil, err
}

// StatFS reports the configured capacity, or caps free space at 1 TiB.
func (e *Engine) StatFS(ctx context.Context) (smb.Space, error) {
	var used float64
	if err := e.db.QueryRowContext(ctx, `SELECT total(size) FROM files WHERE directory = 0`).Scan(&used); err != nil {
		return smb.Space{}, storageError(err)
	}
	total, free := uint64(used)+1<<40, uint64(1)<<40
	if e.capacity != 0 {
		total = e.capacity
		free = total - min(total, uint64(used))
	}
	return smb.Space{VolumeID: e.volumeID, Capacity: total, Free: free, Available: free}, nil
}
