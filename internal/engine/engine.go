// SPDX-License-Identifier: AGPL-3.0-only

// Package engine keeps Time Machine's files as immutable chunk objects in S3.
// A local SQLite database maps files to chunks, and full copies of that
// database go to S3 every 15 minutes. It implements smb.Storage.
//
// Every chunk upload gets a new random name that is never reused. FLUSH
// uploads dirty chunks, then makes one SQLite commit that puts the new chunks
// in and the replaced ones in the trash, and only then replies. A trashed
// chunk is deleted once the oldest of the 4 kept copies was captured after it
// was trashed, so every kept copy can still be restored.
//
// One server owns a bucket, through a lock key that it renews every minute.
// The lease is checked before every commit and every S3 write or delete, and
// once it expires the engine stops for good. So does a copy that has not
// landed for 30 minutes. Dead reports both.
package engine

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/djosh34/s3-smb/internal/smb"
)

// Options configures Open. Dir is the data folder (storage.state_dir). The
// caller must hold its folder lock before Open and until Shutdown returns.
// Capacity is the reported volume size in bytes; zero caps free space at
// 1 TiB. ReadOnly refuses every change to files. Logger may be nil.
type Options struct {
	Bucket   *Bucket
	Logger   *slog.Logger
	Dir      string
	Capacity uint64
	ReadOnly bool
}

// tuning holds the fixed rules. Tests shorten them.
type tuning struct {
	// hook, when set, runs at each named local step. An error there stops the
	// engine as a crash would.
	hook         func(step string) error
	chunkSize    uint64
	dirtyChunks  int
	copiesKept   int
	copyInterval time.Duration
	stopAge      time.Duration
	renewEvery   time.Duration
	lease        time.Duration
	stale        time.Duration
	lockRetry    time.Duration
	copyRetry    time.Duration
	watchEvery   time.Duration
}

func defaultTuning() tuning {
	return tuning{
		chunkSize:    8 << 20,
		dirtyChunks:  (256 << 20) / (8 << 20),
		copiesKept:   4,
		copyInterval: 15 * time.Minute,
		stopAge:      30 * time.Minute,
		renewEvery:   time.Minute,
		lease:        8 * time.Minute,
		stale:        10 * time.Minute,
		lockRetry:    30 * time.Second,
		copyRetry:    10 * time.Second,
		watchEvery:   time.Second,
	}
}

// Local steps that the tests crash at.
const (
	stepCommit          = "commit"
	stepFlushReply      = "flush reply"
	stepPending         = "pending recorded"
	stepCopyAttempt     = "copy attempt recorded"
	stepRestoreRemoved  = "restore removed old files"
	stepRestoreRenamed  = "restore renamed the download"
	stepTrashDeleted    = "trash object deleted"
	stepCopyCaptured    = "copy captured"
	stepStartedTimeline = "timeline started"
	stepCheck           = "before a commit or S3 write"
)

var errLeaseExpired = errors.New("the bucket lease expired; another server may own the bucket now")

// Engine is the storage engine. Its methods are safe for concurrent use.
type Engine struct {
	newest     time.Time // capture time of the newest copy this run landed, or of the start copy before it lands
	leaseUntil time.Time
	objs       objects
	failErr    error
	log        *slog.Logger
	db         *sql.DB
	inodes     map[smb.Inode]*inode
	dead       chan struct{}
	cancel     context.CancelFunc // stops the loops
	dir        string
	lockKey    string
	startCopy  string
	dirty      []*dirtyChunk
	tune       tuning
	group      sync.WaitGroup
	capacity   uint64
	volumeID   uint64
	admitting  int   // dirty chunks admitted but not yet added, guarded by mu
	captureSeq int64 // highest copy sequence whose capture has started
	startSeen  int64 // highest copy sequence seen at start
	failOnce   sync.Once
	commitMu   sync.Mutex // serializes commits and copy captures
	copyMu     sync.Mutex // one copy at a time
	mu         sync.Mutex // inodes, dirty and admitting
	timesMu    sync.Mutex // newest and leaseUntil
	readOnly   bool
}

var _ smb.Storage = (*Engine)(nil)

// Open runs the start sequence: take the bucket lock, waiting while another
// server holds it; list the copies; keep the local database or restore the
// newest copy; start a new timeline; upload the start copy, numbered two
// above the highest sequence seen. It returns once the engine can serve.
func Open(ctx context.Context, options Options) (*Engine, error) {
	if options.Bucket == nil {
		return nil, errors.New("a bucket is required")
	}
	return open(ctx, options, options.Bucket, defaultTuning())
}

func open(ctx context.Context, options Options, objs objects, tune tuning) (*Engine, error) {
	if !filepath.IsAbs(options.Dir) {
		return nil, errors.New("the data folder must be an absolute path")
	}
	logger := options.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	e := &Engine{
		objs: objs, log: logger, dir: options.Dir, tune: tune, capacity: options.Capacity, readOnly: options.ReadOnly,
		inodes: make(map[smb.Inode]*inode), dead: make(chan struct{}),
	}
	loops, cancel := context.WithCancel(context.WithoutCancel(ctx))
	e.cancel = cancel
	serverID, err := loadServerID(options.Dir)
	if err != nil {
		return nil, err
	}
	if err = e.takeLock(ctx, serverID); err != nil {
		cancel()
		return nil, err
	}
	e.group.Add(2)
	go e.renewLoop(loops)
	go e.watch(loops)
	if err = e.start(ctx); err != nil {
		return nil, errors.Join(err, e.shutdown(context.WithoutCancel(ctx)))
	}
	e.group.Add(1)
	go e.copyLoop(loops)
	return e, nil
}

func (e *Engine) start(ctx context.Context) error {
	copies, err := e.listCopies(ctx)
	if err != nil {
		return err
	}
	newest, err := e.prepareDatabase(ctx, copies)
	if err != nil {
		return err
	}
	if e.db, err = openDatabase(ctx, e.dir); err != nil {
		return fmt.Errorf("open the database: %w", err)
	}
	// Highest seen counts the attempts this database recorded too, so an
	// attempt that may still land never shares the start copy's sequence.
	var highest int64
	if err = e.db.QueryRowContext(ctx, `SELECT coalesce(max(seq), 0) FROM copies`).Scan(&highest); err != nil {
		return storageError(err)
	}
	for _, c := range copies {
		highest = max(highest, c.seq)
	}
	e.captureSeq, e.startSeen = highest, highest
	if err = e.newTimeline(ctx, highest, newest); err != nil {
		return err
	}
	state, err := readState(ctx, e.db)
	if err != nil {
		return err
	}
	if e.volumeID, err = volumeIdentity(state.volume); err != nil {
		return err
	}
	if err = e.makeCopy(ctx, highest+2); err != nil {
		return fmt.Errorf("upload the start copy: %w", err)
	}
	return nil
}

// newTimeline gives the database a new history ID and sends what no row uses
// to the trash: pending early uploads and files unlinked while open. It
// records newest, the copy the database was kept against or restored from,
// as published.
func (e *Engine) newTimeline(ctx context.Context, highest int64, newest copyName) error {
	history, err := randomID()
	if err != nil {
		return err
	}
	err = e.commit(ctx, func(tx *sql.Tx) error {
		return execAll(ctx, tx, []statement{
			{
				`UPDATE state SET history = ?,
				published = coalesce(nullif(?, ''), published),
				published_commits = CASE WHEN ? = '' THEN published_commits ELSE ? END WHERE id = 1`,
				[]any{history, newest.history, newest.history, newest.counter},
			},
			{`INSERT INTO trash (name, seq) SELECT name, ? FROM pending`, []any{highest}},
			{`DELETE FROM pending`, nil},
			{`INSERT INTO trash (name, seq) SELECT c.name, ? FROM chunks c JOIN files f ON f.id = c.file
				WHERE f.parent IS NULL AND f.id != ?`, []any{highest, rootInode}},
			{`DELETE FROM chunks WHERE file IN (SELECT id FROM files WHERE parent IS NULL AND id != ?)`, []any{rootInode}},
			{`DELETE FROM files WHERE parent IS NULL AND id != ?`, []any{rootInode}},
		})
	})
	if err != nil {
		return err
	}
	return e.step(stepStartedTimeline)
}

type statement struct {
	query string
	args  []any
}

func execAll(ctx context.Context, tx *sql.Tx, statements []statement) error {
	for _, s := range statements {
		if _, err := tx.ExecContext(ctx, s.query, s.args...); err != nil {
			return err
		}
	}
	return nil
}

// prepareDatabase keeps the local database only if it passes quick_check,
// has the history of the newest copy and is not behind it. Otherwise it
// restores the newest copy. With no copy at all, a missing database is
// created fresh and a damaged one stops the start. It returns the newest
// copy, with an empty key when there is none.
func (e *Engine) prepareDatabase(ctx context.Context, copies []copyName) (copyName, error) {
	path := filepath.Join(e.dir, databaseName)
	local, err := inspectLocal(ctx, path)
	if err != nil {
		return copyName{}, err
	}
	newest := pickNewest(copies, local)
	switch {
	case newest == nil && local.damaged:
		return copyName{}, fmt.Errorf("the local database is damaged and the bucket has no copy: %w", local.problem)
	case newest == nil:
		return copyName{}, nil
	case local.holds(*newest):
		e.log.Info("keeping the local database", "copy", newest.key)
		return *newest, nil
	}
	e.log.Warn("restoring the newest database copy", "copy", newest.key, "local_found", local.ok || local.damaged, "local_problem", local.problem)
	return *newest, e.restore(ctx, *newest)
}

// localDatabase is what the start sees of the local database.
type localDatabase struct {
	problem error
	state   stateRow
	ok      bool // found and passed the check
	damaged bool // found but failed the check
}

// holds reports a good local database that holds copy c: c is of its
// current history and not ahead of it, or of the history it last published
// and not ahead of that copy. Each history is one database's line of
// commits, so only counters of the same history compare.
func (l localDatabase) holds(c copyName) bool {
	return l.ok && (c.history == l.state.history && c.counter <= l.state.commits ||
		c.history == l.state.published && c.counter <= l.state.publishedCommits)
}

func inspectLocal(ctx context.Context, path string) (localDatabase, error) {
	if missing, err := missingDatabase(path); missing || err != nil {
		return localDatabase{}, err
	}
	// Open read-write so SQLite can replay a WAL left by a crash.
	db := openSQLite(path, "rw")
	problem := checkDatabase(ctx, db)
	var state stateRow
	if problem == nil {
		state, problem = readState(ctx, db)
	}
	if err := errors.Join(db.Close(), ctx.Err()); err != nil {
		return localDatabase{}, err
	}
	if problem != nil {
		return localDatabase{damaged: true, problem: problem}, nil //nolint:nilerr // A damaged database is a finding for the caller.
	}
	return localDatabase{ok: true, state: state}, nil
}

// pickNewest returns the copy with the highest sequence. On a tie, a copy of
// the local history wins if the local database is not behind it, and
// otherwise the copy with more commits.
func pickNewest(copies []copyName, local localDatabase) *copyName {
	var newest *copyName
	for i := range copies {
		c := &copies[i]
		switch {
		case newest == nil || c.seq > newest.seq:
			newest = c
		case c.seq < newest.seq:
		case local.holds(*c):
			newest = c
		case local.holds(*newest):
		case c.counter > newest.counter || c.counter == newest.counter && c.history > newest.history:
			newest = c
		}
	}
	return newest
}

// restore downloads a copy, checks it, then installs it: delete db, db-wal
// and db-shm, sync the folder, rename the download in and sync again. A
// leftover WAL could otherwise replay old commits over the copy.
func (e *Engine) restore(ctx context.Context, c copyName) error {
	data, err := e.objs.get(ctx, c.key, 0, 0)
	if err != nil {
		return fmt.Errorf("download copy %s: %w", c.key, err)
	}
	temp := filepath.Join(e.dir, restoreTemp)
	if err = writeSynced(temp, data); err != nil {
		return err
	}
	if err = checkFile(ctx, temp); err != nil {
		return fmt.Errorf("copy %s is damaged: %w", c.key, err)
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err = os.Remove(filepath.Join(e.dir, databaseName+suffix)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err = syncDir(e.dir); err != nil {
		return err
	}
	if err = e.step(stepRestoreRemoved); err != nil {
		return err
	}
	if err = os.Rename(temp, filepath.Join(e.dir, databaseName)); err != nil {
		return err
	}
	if err = syncDir(e.dir); err != nil {
		return err
	}
	return e.step(stepRestoreRenamed)
}

func writeSynced(path string, data []byte) error {
	file, err := os.OpenFile(filepath.Clean(path), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = file.Write(data)
	if err == nil {
		err = file.Sync()
	}
	return errors.Join(err, file.Close())
}

func syncDir(dir string) error {
	file, err := os.Open(filepath.Clean(dir))
	if err != nil {
		return err
	}
	return errors.Join(file.Sync(), file.Close())
}

// Dead is closed once the engine has stopped for good: the lease expired, a
// copy has not landed for 30 minutes, or a crash step fired in a test. Err
// then says why. The server must exit.
func (e *Engine) Dead() <-chan struct{} { return e.dead }

// Err returns why the engine died, or nil while it lives.
func (e *Engine) Err() error {
	select {
	case <-e.dead:
		return e.failErr
	default:
		return nil
	}
}

func (e *Engine) fail(err error) {
	e.failOnce.Do(func() {
		e.failErr = err
		close(e.dead)
		e.cancel()
		e.log.Error("storage engine stopped", "error", err)
	})
}

// check runs before every commit and every S3 write or delete.
func (e *Engine) check() error {
	if err := e.Err(); err != nil {
		return fmt.Errorf("%w: %w", smb.ErrIO, err)
	}
	e.timesMu.Lock()
	until := e.leaseUntil
	e.timesMu.Unlock()
	if passed(until) {
		e.fail(errLeaseExpired)
		return fmt.Errorf("%w: %w", smb.ErrIO, errLeaseExpired)
	}
	return e.step(stepCheck)
}

// passed reports whether deadline has passed by the monotonic clock or by
// the wall clock. The monotonic clock stops while the host sleeps, the wall
// clock does not.
func passed(deadline time.Time) bool {
	now := time.Now()
	return !now.Before(deadline) || !now.Round(0).Before(deadline.Round(0))
}

func (e *Engine) step(name string) error {
	if e.tune.hook == nil {
		return nil
	}
	if err := e.tune.hook(name); err != nil {
		e.fail(err)
		return fmt.Errorf("%w: %w", smb.ErrIO, err)
	}
	return nil
}

func (e *Engine) put(ctx context.Context, key string, data []byte) error {
	if err := e.check(); err != nil {
		return err
	}
	return e.objs.put(ctx, key, data)
}

func (e *Engine) remove(ctx context.Context, key string) error {
	if err := e.check(); err != nil {
		return err
	}
	return e.objs.remove(ctx, key)
}

// commit runs fn in one SQLite transaction and counts it in the state row.
func (e *Engine) commit(ctx context.Context, fn func(tx *sql.Tx) error) error {
	e.commitMu.Lock()
	defer e.commitMu.Unlock()
	if err := e.check(); err != nil {
		return err
	}
	tx, err := e.db.BeginTx(ctx, nil)
	if err != nil {
		return storageError(err)
	}
	err = fn(tx)
	if err == nil {
		_, err = tx.ExecContext(ctx, `UPDATE state SET commits = commits + 1 WHERE id = 1`)
	}
	if err == nil {
		// The work above can outlast the lease.
		err = e.check()
	}
	if err != nil {
		return storageError(errors.Join(err, tx.Rollback()))
	}
	if err = tx.Commit(); err != nil {
		return storageError(err)
	}
	return e.step(stepCommit)
}

// Shutdown flushes what is still dirty, stops the engine and deletes its
// lock key. Call it after the server has closed every handle.
func (e *Engine) Shutdown(ctx context.Context) error {
	err := e.flushAll(ctx)
	return errors.Join(err, e.shutdown(ctx))
}

func (e *Engine) shutdown(ctx context.Context) error {
	e.cancel()
	e.group.Wait()
	var err error
	if e.db != nil {
		err = e.db.Close()
	}
	// A clean exit deletes its own key, so the next start need not wait.
	if e.check() == nil {
		err = errors.Join(err, e.objs.remove(ctx, e.lockKey))
	}
	e.fail(errors.New("the storage engine is closed"))
	return err
}

// sleep waits for d, or returns false when ctx ends first. The loops' ctx
// ends when the engine stops.
func sleep(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// watch stops the engine when the lease expires, and when the newest copy
// this run landed was captured more than 30 minutes ago, even while an
// upload hangs.
func (e *Engine) watch(ctx context.Context) {
	defer e.group.Done()
	for sleep(ctx, e.tune.watchEvery) {
		e.timesMu.Lock()
		newest, until := e.newest, e.leaseUntil
		e.timesMu.Unlock()
		if passed(until) {
			e.fail(errLeaseExpired)
			return
		}
		if !newest.IsZero() && passed(newest.Add(e.tune.stopAge)) {
			e.fail(fmt.Errorf("the newest database copy in S3 was captured more than %s ago; stopping so no more recent backup can be lost", e.tune.stopAge))
			return
		}
	}
}

func randomID() (string, error) {
	var b [16]byte
	if _, err := io.ReadFull(rand.Reader, b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// storageError keeps error kinds and cancellation, and files everything else
// under ErrIO, keeping the cause for logs.
func storageError(err error) error {
	if err == nil || errors.Is(err, io.EOF) {
		return err
	}
	var kind smb.ErrorKind
	if errors.As(err, &kind) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if errors.Is(err, syscall.ENOSPC) {
		return fmt.Errorf("%w: %w", smb.ErrDiskFull, err)
	}
	return fmt.Errorf("%w: %w", smb.ErrIO, err)
}
