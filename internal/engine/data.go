// SPDX-License-Identifier: AGPL-3.0-only
package engine

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"maps"
	"slices"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/djosh34/s3-smb/internal/smb"
)

// maxFileSize keeps chunk offsets far inside int64.
const maxFileSize = uint64(1) << 50

// uploads is how many chunks one FLUSH uploads at once.
const uploads = 4

// inode is the in-memory state of a file that is open, pinned or has data
// not yet committed. mu orders I/O on the file and guards dirty and early.
// Engine.mu guards users, refs and unlinked, so CLOSE never waits for I/O.
// liveMu guards live, so attribute reads never wait for I/O either.
type inode struct {
	dirty    map[uint64]*dirtyChunk
	early    map[uint64]earlyChunk
	live     live
	id       smb.Inode
	users    int // pins
	refs     int // open handles
	mu       sync.Mutex
	liveMu   sync.Mutex
	unlinked bool
}

// live is what a file looks like with its writes that are not yet committed.
type live struct {
	modified   time.Time
	changed    time.Time
	size       uint64
	loaded     bool // size is current
	timesDirty bool // writes changed the times since the last commit
}

// dirtyChunk is a chunk's bytes waiting in RAM. data never reaches past the
// file's size; bytes after it read as zeros.
type dirtyChunk struct {
	owner *inode
	data  []byte
	idx   uint64
}

// earlyChunk is a chunk uploaded before FLUSH to make room in RAM. A pending
// row names it until a commit settles it.
type earlyChunk struct {
	name   string
	length uint64
}

func (st *inode) snapshot() live {
	st.liveMu.Lock()
	defer st.liveMu.Unlock()
	return st.live
}

func (st *inode) update(fn func(*live)) {
	st.liveMu.Lock()
	fn(&st.live)
	st.liveMu.Unlock()
}

func (st *inode) idle() bool {
	return st.refs == 0 && len(st.dirty) == 0 && len(st.early) == 0 && !st.unlinked
}

// pin returns the inode's state without its I/O lock.
func (e *Engine) pin(id smb.Inode) (*inode, func()) {
	e.mu.Lock()
	st := e.inodes[id]
	if st == nil {
		st = &inode{id: id, dirty: make(map[uint64]*dirtyChunk), early: make(map[uint64]earlyChunk)}
		e.inodes[id] = st
	}
	st.users++
	e.mu.Unlock()
	return st, func() { e.unpin(st) }
}

func (e *Engine) unpin(st *inode) {
	e.mu.Lock()
	defer e.mu.Unlock()
	st.users--
	// Nothing else can hold the I/O lock without a pin.
	if st.users == 0 && st.mu.TryLock() {
		if st.idle() {
			delete(e.inodes, st.id)
		}
		st.mu.Unlock()
	}
}

func (e *Engine) acquire(id smb.Inode) (*inode, func()) {
	st, unpin := e.pin(id)
	st.mu.Lock()
	return st, func() { st.mu.Unlock(); unpin() }
}

// load reads the committed size once. The I/O lock must be held.
func (e *Engine) load(ctx context.Context, st *inode) (row, error) {
	r, err := e.fileRow(ctx, st.id)
	if err == nil {
		st.update(func(l *live) {
			if !l.loaded {
				l.size, l.loaded = r.size, true
			}
		})
	}
	return r, err
}

type handle struct {
	st     *inode
	key    smb.Inode
	access smb.Access
	kind   smb.Kind
	closed bool // guarded by Engine.mu
}

func (h *handle) Key() smb.Inode { return h.key }

func (e *Engine) selected(ctx context.Context, ref smb.Handle, write bool) (*handle, func(), error) {
	h, ok := ref.(*handle)
	if !ok || h == nil {
		return nil, nil, smb.ErrInvalidHandle
	}
	st, release := e.acquire(h.key)
	e.mu.Lock()
	closed := h.closed
	e.mu.Unlock()
	if st != h.st || closed {
		release()
		return nil, nil, smb.ErrInvalidHandle
	}
	if err := ctx.Err(); err != nil {
		release()
		return nil, nil, err
	}
	if write && e.readOnly {
		release()
		return nil, nil, smb.ErrReadOnly
	}
	if write && h.access&(smb.AccessWrite|smb.AccessAppend) == 0 {
		release()
		return nil, nil, smb.ErrAccessDenied
	}
	return h, release, nil
}

// Open returns a handle on an existing file or directory. It does not wait
// for the file's I/O, which an upload can hold for a whole S3 outage, and
// CREATE opens a file while it guards the file's folder.
func (e *Engine) Open(ctx context.Context, ino smb.Inode, access smb.Access) (smb.Handle, error) {
	if access&^(smb.AccessRead|smb.AccessWrite|smb.AccessAppend) != 0 || ino == 0 {
		return nil, smb.ErrInvalidParameter
	}
	if e.readOnly && access&(smb.AccessWrite|smb.AccessAppend) != 0 {
		return nil, smb.ErrReadOnly
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	st, unpin := e.pin(ino)
	defer unpin()
	r, err := e.fileRow(ctx, ino)
	if err != nil {
		return nil, err
	}
	// Only a handle changes a file's live size, so before the first one the
	// committed size is current.
	st.update(func(l *live) {
		if !l.loaded {
			l.size, l.loaded = r.size, true
		}
	})
	kind := smb.KindFile
	if r.directory {
		kind = smb.KindDirectory
	}
	e.mu.Lock()
	st.refs++
	e.mu.Unlock()
	return &handle{st: st, key: ino, access: access, kind: kind}, nil
}

// Close releases the handle, even when ctx is canceled. Dirty data waits for
// the next FLUSH: CLOSE promises nothing about durability. It must never
// wait on S3, so it does not wait for the file's I/O, which can. The last
// close of an unlinked file drops it once its I/O lock is free.
func (e *Engine) Close(ctx context.Context, ref smb.Handle) error {
	h, ok := ref.(*handle)
	if !ok || h == nil {
		return smb.ErrInvalidHandle
	}
	st, unpin := e.pin(h.key)
	defer unpin()
	e.mu.Lock()
	if st != h.st || h.closed {
		e.mu.Unlock()
		return smb.ErrInvalidHandle
	}
	h.closed = true
	st.refs--
	last := st.refs == 0 && st.unlinked
	e.mu.Unlock()
	var err error
	if last {
		err = e.dropWhenFree(ctx, st)
	}
	return errors.Join(err, ctx.Err())
}

// dropWhenFree drops an unlinked file that nothing has open. It does not wait
// for the file's I/O lock, which an upload can hold for a whole S3 outage:
// while the lock is busy, a goroutine drops the file once it is free. A crash
// before that leaves the unlinked row, which the next start drops.
func (e *Engine) dropWhenFree(ctx context.Context, st *inode) error {
	ctx = context.WithoutCancel(ctx)
	if st.mu.TryLock() {
		defer st.mu.Unlock()
		return e.dropUnlinked(ctx, st)
	}
	_, unpin := e.pin(st.id)
	e.group.Go(func() {
		defer unpin()
		st.mu.Lock()
		defer st.mu.Unlock()
		if err := e.dropUnlinked(ctx, st); err != nil && e.Err() == nil {
			e.log.Error("dropping an unlinked file failed; the next start drops it", "error", err)
		}
	})
	return nil
}

// dropUnlinked drops an unlinked file that nothing has open anymore. The I/O
// lock must be held.
func (e *Engine) dropUnlinked(ctx context.Context, st *inode) error {
	e.mu.Lock()
	drop := st.refs == 0 && st.unlinked
	e.mu.Unlock()
	if !drop {
		return nil
	}
	if err := e.commit(ctx, func(tx *sql.Tx) error { return e.dropFile(ctx, tx, st.id, st) }); err != nil {
		return err
	}
	e.mu.Lock()
	st.unlinked = false
	e.mu.Unlock()
	e.discard(st)
	return nil
}

// discard forgets a dropped file's data. The I/O lock must be held.
func (e *Engine) discard(st *inode) {
	for _, c := range st.dirty {
		e.forget(c)
	}
	clear(st.dirty)
	clear(st.early)
	st.update(func(l *live) { *l = live{} })
}

func (e *Engine) forget(c *dirtyChunk) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if i := slices.Index(e.dirty, c); i >= 0 {
		e.dirty = slices.Delete(e.dirty, i, i+1)
	}
	delete(c.owner.dirty, c.idx)
}

// Flush returns once the file's writes are in S3 and committed. SyncFull
// does this for every file.
func (e *Engine) Flush(ctx context.Context, ref smb.Handle, mode smb.SyncMode) error {
	if mode != smb.SyncData && mode != smb.SyncFull {
		return smb.ErrInvalidParameter
	}
	h, release, err := e.selected(ctx, ref, false)
	if err != nil {
		return err
	}
	if mode == smb.SyncFull {
		release()
		err = e.flushAll(ctx)
	} else {
		err = e.flush(ctx, h.st)
		release()
	}
	if err != nil {
		return err
	}
	return e.step(stepFlushReply)
}

// flushAll flushes every file with uncommitted data.
func (e *Engine) flushAll(ctx context.Context) error {
	e.mu.Lock()
	ids := slices.Collect(maps.Keys(e.inodes))
	e.mu.Unlock()
	slices.Sort(ids)
	var result error
	for _, id := range ids {
		st, release := e.acquire(id)
		result = errors.Join(result, e.flush(ctx, st))
		release()
	}
	return result
}

type upload struct {
	name   string
	length uint64
}

// flush uploads the dirty chunks, then makes one commit: new chunks in,
// replaced chunks to the trash, early uploads settled, size and times
// updated. If the uploads or the commit fail, what did upload goes to the
// trash, so it is not left in S3 for good. The I/O lock must be held.
func (e *Engine) flush(ctx context.Context, st *inode) error {
	l := st.snapshot()
	if len(st.dirty) == 0 && len(st.early) == 0 && !l.timesDirty {
		return nil
	}
	uploaded, err := e.uploadDirty(ctx, st)
	if err == nil {
		err = e.commit(ctx, func(tx *sql.Tx) error { return e.settle(ctx, tx, st, uploaded, l) })
	}
	if err != nil {
		return errors.Join(err, e.trashUploads(ctx, uploaded))
	}
	for _, c := range st.dirty {
		e.forget(c)
	}
	clear(st.early)
	st.update(func(l *live) { l.timesDirty = false })
	return nil
}

func (e *Engine) settle(ctx context.Context, tx *sql.Tx, st *inode, uploaded map[uint64]upload, l live) error {
	var statements []statement
	for idx, u := range uploaded {
		statements = append(statements, replaceChunk(st.id, idx, u, e.captureSeq)...)
	}
	for idx, early := range st.early {
		if _, replaced := uploaded[idx]; replaced {
			statements = append(statements, trashPending(early.name, e.captureSeq)...)
		} else {
			statements = append(statements, statement{`DELETE FROM pending WHERE name = ?`, []any{early.name}})
			statements = append(statements, replaceChunk(st.id, idx, upload(early), e.captureSeq)...)
		}
	}
	statements = append(statements, statement{`UPDATE files SET size = ? WHERE id = ?`, []any{l.size, st.id}})
	if l.timesDirty {
		statements = append(statements, statement{
			`UPDATE files SET modified = ?, changed = ? WHERE id = ?`,
			[]any{timeValue(l.modified), timeValue(l.changed), st.id},
		})
	}
	return execAll(ctx, tx, statements)
}

func (e *Engine) trashUploads(ctx context.Context, uploaded map[uint64]upload) error {
	if len(uploaded) == 0 {
		return nil
	}
	ctx = context.WithoutCancel(ctx)
	return e.commit(ctx, func(tx *sql.Tx) error {
		for _, u := range uploaded {
			if _, err := tx.ExecContext(ctx, `INSERT INTO trash (name, seq) VALUES (?, ?)`, u.name, e.captureSeq); err != nil {
				return err
			}
		}
		return nil
	})
}

// replaceChunk points a chunk index at a new object and trashes the old one.
func replaceChunk(file smb.Inode, idx uint64, u upload, seq int64) []statement {
	return []statement{
		{`INSERT INTO trash (name, seq) SELECT name, ? FROM chunks WHERE file = ? AND idx = ?`, []any{seq, file, idx}},
		{`INSERT OR REPLACE INTO chunks (file, idx, name, length) VALUES (?, ?, ?, ?)`, []any{file, idx, u.name, u.length}},
	}
}

// uploadDirty puts each dirty chunk under a new random name. It returns what
// did upload, also on error.
func (e *Engine) uploadDirty(ctx context.Context, st *inode) (map[uint64]upload, error) {
	names := make(map[uint64]string, len(st.dirty))
	for idx := range st.dirty {
		name, err := randomID()
		if err != nil {
			return nil, err
		}
		names[idx] = name
	}
	result := make(map[uint64]upload, len(st.dirty))
	var mu sync.Mutex
	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(uploads)
	for idx, c := range st.dirty {
		group.Go(func() error {
			if err := e.put(groupCtx, chunkPrefix+names[idx], c.data); err != nil {
				return storageError(err)
			}
			mu.Lock()
			result[idx] = upload{name: names[idx], length: uint64(len(c.data))}
			mu.Unlock()
			return nil
		})
	}
	return result, group.Wait()
}

// admit reserves room for one more dirty chunk of st, whose I/O lock is
// held. While the budget is full, it uploads the oldest dirty chunk early:
// one of st, or of a file whose lock is free. When every chunk belongs to a
// busy file it waits. That file's holder is not waiting here, since it would
// evict its own chunks, so it finishes and frees the lock. The caller ends
// the reservation by adding the chunk, or giving up, under Engine.mu.
func (e *Engine) admit(ctx context.Context, st *inode) error {
	for {
		e.mu.Lock()
		if len(e.dirty)+e.admitting < e.tune.dirtyChunks {
			e.admitting++
			e.mu.Unlock()
			return nil
		}
		var victim *dirtyChunk
		for _, c := range e.dirty {
			if c.owner == st {
				victim = c
				break
			}
			if c.owner.mu.TryLock() {
				c.owner.users++
				victim = c
				break
			}
		}
		e.mu.Unlock()
		if victim == nil {
			if !sleep(ctx, time.Millisecond) {
				return ctx.Err()
			}
			continue
		}
		err := e.evict(ctx, victim.owner, victim)
		if victim.owner != st {
			victim.owner.mu.Unlock()
			e.unpin(victim.owner)
		}
		if err != nil {
			return err
		}
	}
}

// evict records a pending row, then uploads the chunk under that name. An
// earlier early upload of the same chunk goes to the trash in that commit.
func (e *Engine) evict(ctx context.Context, st *inode, c *dirtyChunk) error {
	name, err := randomID()
	if err != nil {
		return err
	}
	old, replaced := st.early[c.idx]
	err = e.commit(ctx, func(tx *sql.Tx) error {
		statements := []statement{{`INSERT INTO pending (name) VALUES (?)`, []any{name}}}
		if replaced {
			statements = append(statements, trashPending(old.name, e.captureSeq)...)
		}
		return execAll(ctx, tx, statements)
	})
	if err != nil {
		return err
	}
	delete(st.early, c.idx)
	if err = e.step(stepPending); err != nil {
		return err
	}
	if err = e.put(ctx, chunkPrefix+name, c.data); err != nil {
		return storageError(err)
	}
	st.early[c.idx] = earlyChunk{name: name, length: uint64(len(c.data))}
	e.forget(c)
	return nil
}

// WriteAt writes into RAM. With AccessAppend it refuses offsets below EOF.
func (e *Engine) WriteAt(ctx context.Context, ref smb.Handle, src []byte, offset uint64) (int, error) {
	h, release, err := e.selected(ctx, ref, true)
	if err != nil {
		return 0, err
	}
	defer release()
	if offset >= maxFileSize || uint64(len(src)) >= maxFileSize-offset {
		return 0, smb.ErrFileTooLarge
	}
	if h.kind == smb.KindDirectory {
		return 0, smb.ErrIsDirectory
	}
	size := h.st.snapshot().size
	if h.access&smb.AccessAppend != 0 && offset < size {
		return 0, smb.ErrAccessDenied
	}
	if len(src) == 0 {
		return 0, nil
	}
	if err = e.write(ctx, h.st, src, offset); err != nil {
		return 0, err
	}
	return len(src), nil
}

func (e *Engine) write(ctx context.Context, st *inode, src []byte, offset uint64) error {
	size := e.tune.chunkSize
	end := offset + uint64(len(src))
	for pos := offset; pos < end; {
		idx, within := pos/size, pos%size
		n := min(size-within, end-pos)
		c, err := e.dirtyChunk(ctx, st, idx, within, n)
		if err != nil {
			return err
		}
		if need := within + n; uint64(len(c.data)) < need {
			c.data = append(c.data, make([]byte, need-uint64(len(c.data)))...)
		}
		copy(c.data[within:], src[pos-offset:pos-offset+n])
		pos += n
	}
	now := wallClock()
	st.update(func(l *live) {
		l.size = max(l.size, end)
		l.modified, l.changed, l.timesDirty = now, now, true
	})
	return nil
}

// dirtyChunk returns chunk idx in RAM. It reads the chunk's current bytes
// first unless the write at within covers all of them.
func (e *Engine) dirtyChunk(ctx context.Context, st *inode, idx, within, n uint64) (*dirtyChunk, error) {
	if c := st.dirty[idx]; c != nil {
		return c, nil
	}
	if err := e.admit(ctx, st); err != nil {
		return nil, err
	}
	var data []byte
	stored, err := e.stored(ctx, st, idx)
	if err == nil && stored.length > 0 && (within > 0 || n < stored.length) {
		data, err = e.readStored(ctx, stored, 0, stored.length)
	}
	c := &dirtyChunk{owner: st, idx: idx, data: data}
	e.mu.Lock()
	e.admitting--
	if err == nil {
		st.dirty[idx] = c
		e.dirty = append(e.dirty, c)
	}
	e.mu.Unlock()
	return c, err
}

// stored returns the uploaded object of a chunk that is not dirty: its early
// upload, else its committed row. A hole has length zero. The length never
// reaches past the live size.
func (e *Engine) stored(ctx context.Context, st *inode, idx uint64) (upload, error) {
	var valid uint64
	if size, start := st.snapshot().size, idx*e.tune.chunkSize; size > start {
		valid = min(size-start, e.tune.chunkSize)
	}
	if early, ok := st.early[idx]; ok {
		return upload{name: early.name, length: min(early.length, valid)}, nil
	}
	var u upload
	err := e.diskError(e.db.QueryRowContext(ctx, `SELECT name, length FROM chunks WHERE file = ? AND idx = ?`, st.id, idx).Scan(&u.name, &u.length))
	if errors.Is(err, sql.ErrNoRows) {
		return upload{}, nil
	}
	if err != nil {
		return upload{}, storageError(err)
	}
	u.length = min(u.length, valid)
	return u, nil
}

// readStored returns a copy of length bytes at offset of an uploaded chunk.
// It fetches the whole chunk into the read cache once.
func (e *Engine) readStored(ctx context.Context, u upload, offset, length uint64) ([]byte, error) {
	data := e.cache.get(u.name)
	if data == nil {
		var err error
		if data, err = e.objs.get(ctx, chunkPrefix+u.name, 0, 0); err != nil {
			return nil, storageError(err)
		}
		e.cache.add(u.name, data, e.tune.readChunks)
	}
	if uint64(len(data)) < offset+length {
		return nil, smb.ErrIO
	}
	return bytes.Clone(data[offset : offset+length]), nil
}

// ReadAt reads RAM, then S3. Holes and bytes past a chunk's length are zero.
func (e *Engine) ReadAt(ctx context.Context, ref smb.Handle, dst []byte, offset uint64) (int, error) {
	h, release, err := e.selected(ctx, ref, false)
	if err != nil {
		return 0, err
	}
	defer release()
	if h.access&smb.AccessRead == 0 {
		return 0, smb.ErrAccessDenied
	}
	if h.kind == smb.KindDirectory {
		return 0, smb.ErrIsDirectory
	}
	if len(dst) == 0 {
		return 0, nil
	}
	size := h.st.snapshot().size
	if offset >= size {
		return 0, io.EOF
	}
	part := dst
	if size-offset < uint64(len(dst)) {
		part = dst[:size-offset]
	}
	if err = e.read(ctx, h.st, part, offset); err != nil {
		return 0, err
	}
	if len(part) < len(dst) {
		return len(part), io.EOF
	}
	return len(part), nil
}

func (e *Engine) read(ctx context.Context, st *inode, dst []byte, offset uint64) error {
	size := e.tune.chunkSize
	end := offset + uint64(len(dst))
	for pos := offset; pos < end; {
		idx, within := pos/size, pos%size
		n := min(size-within, end-pos)
		part := dst[pos-offset : pos-offset+n]
		clear(part)
		if c := st.dirty[idx]; c != nil {
			if within < uint64(len(c.data)) {
				copy(part, c.data[within:])
			}
		} else {
			stored, err := e.stored(ctx, st, idx)
			if err != nil {
				return err
			}
			if within < stored.length {
				data, err := e.readStored(ctx, stored, within, min(n, stored.length-within))
				if err != nil {
					return err
				}
				copy(part, data)
			}
		}
		pos += n
	}
	return nil
}

// Truncate sets the length. Chunks past it go to the trash in the same
// commit, and the last chunk's length is cut so its tail reads as zeros.
func (e *Engine) Truncate(ctx context.Context, ref smb.Handle, size uint64) error {
	h, release, err := e.selected(ctx, ref, true)
	if err != nil {
		return err
	}
	defer release()
	if h.access&smb.AccessWrite == 0 {
		return smb.ErrAccessDenied
	}
	return e.truncate(ctx, h.st, size)
}

func (e *Engine) truncate(ctx context.Context, st *inode, size uint64) error {
	if size >= maxFileSize {
		return smb.ErrFileTooLarge
	}
	r, err := e.load(ctx, st)
	if err != nil {
		return err
	}
	if r.directory {
		return smb.ErrIsDirectory
	}
	// A size the file already has needs no commit, unless only RAM has it:
	// a truncate is durable at once, also to the size a write gave the file.
	if size == st.snapshot().size && size == r.size {
		return nil
	}
	// Chunks with an index below keep stay. The last of them, keep-1, keeps
	// only its first cut bytes.
	chunk := e.tune.chunkSize
	keep := (size + chunk - 1) / chunk
	cut := size % chunk
	if cut == 0 {
		cut = chunk
	}
	now := wallClock()
	err = e.commit(ctx, func(tx *sql.Tx) error {
		statements := []statement{
			{`INSERT INTO trash (name, seq) SELECT name, ? FROM chunks WHERE file = ? AND idx >= ?`, []any{e.captureSeq, st.id, keep}},
			{`DELETE FROM chunks WHERE file = ? AND idx >= ?`, []any{st.id, keep}},
			{`UPDATE chunks SET length = min(length, ?) WHERE file = ? AND idx = ?`, []any{cut, st.id, keep - 1}},
			{`UPDATE files SET size = ?, modified = ?, changed = ? WHERE id = ?`, []any{size, timeValue(now), timeValue(now), st.id}},
		}
		if keep == 0 {
			statements = slices.Delete(statements, 2, 3)
		}
		for idx, early := range st.early {
			if idx >= keep {
				statements = append(statements, trashPending(early.name, e.captureSeq)...)
			}
		}
		return execAll(ctx, tx, statements)
	})
	if err != nil {
		return err
	}
	for idx, c := range st.dirty {
		switch {
		case idx >= keep:
			e.forget(c)
		case idx+1 == keep && uint64(len(c.data)) > cut:
			c.data = c.data[:cut]
		}
	}
	for idx, early := range st.early {
		switch {
		case idx >= keep:
			delete(st.early, idx)
		case idx+1 == keep && early.length > cut:
			st.early[idx] = earlyChunk{name: early.name, length: cut}
		}
	}
	st.update(func(l *live) {
		l.size = size
		if l.timesDirty {
			l.modified, l.changed = now, now
		}
	})
	return nil
}

// GetAttr returns live attributes without waiting for the file's I/O.
func (e *Engine) GetAttr(ctx context.Context, ino smb.Inode) (smb.Attr, error) {
	st, unpin := e.pin(ino)
	defer unpin()
	r, err := e.fileRow(ctx, ino)
	if err != nil {
		return smb.Attr{}, err
	}
	return attr(r, st), nil
}

// SetAttr applies a size change as Truncate does, then times and attributes
// in one commit. Explicit times also replace the live write times, so a
// later FLUSH keeps them.
func (e *Engine) SetAttr(ctx context.Context, ino smb.Inode, change smb.AttrChange) error {
	if e.readOnly {
		return smb.ErrReadOnly
	}
	if change.Size != nil && change.SizeCap != nil {
		return smb.ErrInvalidParameter
	}
	if change.Size != nil && *change.Size >= maxFileSize || change.SizeCap != nil && *change.SizeCap >= maxFileSize {
		return smb.ErrFileTooLarge
	}
	st, release := e.acquire(ino)
	defer release()
	r, err := e.load(ctx, st)
	if err != nil {
		return err
	}
	if change.SizeCap != nil {
		if r.directory {
			return smb.ErrIsDirectory
		}
		if *change.SizeCap < st.snapshot().size {
			change.Size = change.SizeCap
		}
	}
	if change.Size != nil {
		if err = e.truncate(ctx, st, *change.Size); err != nil {
			return err
		}
	}
	var statements []statement
	for _, field := range []struct {
		value  *time.Time
		column string
	}{{change.Created, "created"}, {change.Accessed, "accessed"}, {change.Modified, "modified"}, {change.Changed, "changed"}} {
		if field.value != nil {
			statements = append(statements, statement{`UPDATE files SET ` + field.column + ` = ? WHERE id = ?`, []any{timeValue(*field.value), r.id}})
		}
	}
	if change.Attributes != nil {
		bits := *change.Attributes &^ attributeDirectory
		if r.directory {
			bits |= attributeDirectory
		}
		statements = append(statements, statement{`UPDATE files SET attributes = ? WHERE id = ?`, []any{bits, r.id}})
	}
	if len(statements) == 0 {
		return nil
	}
	if err = e.commit(ctx, func(tx *sql.Tx) error { return execAll(ctx, tx, statements) }); err != nil {
		return err
	}
	st.update(func(l *live) {
		if change.Modified != nil {
			l.modified = timeOf(timeValue(*change.Modified))
		}
		if change.Changed != nil {
			l.changed = timeOf(timeValue(*change.Changed))
		}
	})
	return nil
}
