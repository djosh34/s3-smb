// SPDX-License-Identifier: AGPL-3.0-only
package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	copyPrefix  = "db/"
	chunkPrefix = "chunks/"
)

// copyName is a database copy, db/<seq>-<counter>-<history>. seq is the copy
// sequence, counter the commit counter read from the copy itself, and
// history the timeline that made it.
type copyName struct {
	key     string
	history string
	seq     int64
	counter int64
}

func formatCopy(seq, counter int64, history string) string {
	return fmt.Sprintf("%s%012d-%012d-%s", copyPrefix, seq, counter, history)
}

func parseCopy(key string) (copyName, bool) {
	fields := strings.Split(strings.TrimPrefix(key, copyPrefix), "-")
	if !strings.HasPrefix(key, copyPrefix) || len(fields) != 3 || !validID(fields[2]) {
		return copyName{}, false
	}
	seq, seqErr := strconv.ParseInt(fields[0], 10, 64)
	counter, counterErr := strconv.ParseInt(fields[1], 10, 64)
	if seqErr != nil || counterErr != nil || seq < 1 || counter < 0 {
		return copyName{}, false
	}
	return copyName{key: key, seq: seq, counter: counter, history: fields[2]}, true
}

// listCopies lists the copies in the bucket. Other keys under db/ are left
// alone.
func (e *Engine) listCopies(ctx context.Context) ([]copyName, error) {
	keys, err := e.objs.list(ctx, copyPrefix)
	if err != nil {
		return nil, err
	}
	var copies []copyName
	for _, k := range keys {
		if c, ok := parseCopy(k.key); ok {
			copies = append(copies, c)
		}
	}
	return copies, nil
}

// copyLoop makes a copy every copyInterval, even when nothing changed.
func (e *Engine) copyLoop(ctx context.Context) {
	defer e.group.Done()
	next := time.Now().Add(e.tune.copyInterval)
	for sleep(ctx, time.Until(next)) {
		next = time.Now().Add(e.tune.copyInterval)
		if err := e.makeCopy(ctx, 0); err != nil && e.Err() == nil && ctx.Err() == nil {
			e.log.Error("database copy failed", "error", err)
		}
	}
}

// makeCopy captures the database into a temp file with VACUUM INTO, records
// the attempt and uploads it as copy seq, or as the next sequence when seq is
// zero. It retries the same bytes until they land or the engine stops; each
// attempt is bounded by the bucket's request timeout. Then it deletes old
// copies and expired trash until the next copy is due.
func (e *Engine) makeCopy(ctx context.Context, seq int64) error {
	e.copyMu.Lock()
	defer e.copyMu.Unlock()
	// VACUUM INTO needs a missing or empty file. An empty one keeps it private.
	temp := filepath.Join(e.dir, copyTemp)
	if err := writeSynced(temp, nil); err != nil {
		return e.diskError(err)
	}
	seq, captured, err := e.capture(ctx, seq, temp)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(filepath.Clean(temp))
	if err != nil {
		return e.diskError(err)
	}
	state, err := readFileState(ctx, temp)
	if err != nil {
		return e.diskError(fmt.Errorf("read the copy's state: %w", err))
	}
	name := formatCopy(seq, state.commits, state.history)
	err = e.commit(ctx, func(tx *sql.Tx) error {
		_, execErr := tx.ExecContext(ctx, `INSERT INTO copies (seq, counter, history, captured, landed) VALUES (?, ?, ?, ?, 0)`,
			seq, state.commits, state.history, timeValue(captured))
		return execErr
	})
	if err == nil {
		err = e.step(stepCopyAttempt)
	}
	if err != nil {
		return err
	}
	for {
		if err = e.put(ctx, name, data); err == nil {
			break
		}
		if e.Err() != nil || ctx.Err() != nil {
			return err
		}
		e.log.Warn("database copy upload failed; retrying the same copy", "copy", name, "error", err)
		if !sleep(ctx, e.tune.copyRetry) {
			return err
		}
	}
	e.timesMu.Lock()
	e.newest = captured
	e.timesMu.Unlock()
	if e.startCopy == "" {
		e.startCopy = name
	}
	e.log.Info("database copy landed", "copy", name)
	err = errors.Join(e.diskError(os.Remove(temp)), e.commit(ctx, func(tx *sql.Tx) error {
		return execAll(ctx, tx, []statement{
			{`UPDATE copies SET landed = 1 WHERE seq = ?`, []any{seq}},
			{`UPDATE state SET published = ?, published_commits = ? WHERE id = 1`, []any{state.history, state.commits}},
		})
	}))
	if err != nil {
		return err
	}
	return e.cleanup(ctx, captured.Add(e.tune.copyInterval))
}

// capture takes copy seq while no commit runs, so a commit either is in the
// copy or stores a trash sequence of at least seq. For the start copy it
// also starts the stop timer, which then covers the upload.
func (e *Engine) capture(ctx context.Context, seq int64, temp string) (int64, time.Time, error) {
	e.commitMu.Lock()
	defer e.commitMu.Unlock()
	if err := e.check(); err != nil {
		return 0, time.Time{}, err
	}
	if seq == 0 {
		seq = e.captureSeq + 1
	}
	e.captureSeq = seq
	captured := time.Now()
	if _, err := e.db.ExecContext(ctx, `VACUUM INTO ?`, temp); err != nil {
		return 0, time.Time{}, e.diskError(fmt.Errorf("capture copy %d: %w", seq, err))
	}
	e.timesMu.Lock()
	if e.newest.IsZero() {
		e.newest = captured
	}
	e.timesMu.Unlock()
	return seq, captured, e.step(stepCopyCaptured)
}

// cleanup keeps the newest copiesKept copy sequences and deletes older
// copies. Then it deletes each trashed chunk whose trash sequence is below
// the oldest kept copy, which was therefore captured after the chunk left
// every row. It counts landed copies, and the clock only bounds how long it
// runs. A chunk that a row or pending upload names is never deleted.
func (e *Engine) cleanup(ctx context.Context, deadline time.Time) error {
	listed, err := e.listCopies(ctx)
	if err != nil {
		return err
	}
	var copies []copyName
	var seqs []int64
	for _, c := range listed {
		// Only a dead run's late upload can land above the highest sequence
		// seen at start and at or below the start copy. Its chunks are not
		// protected by this timeline's trash, so it goes.
		if c.seq > e.startSeen && c.seq <= e.startSeen+2 && c.key != e.startCopy {
			if err = e.remove(ctx, c.key); err != nil {
				return err
			}
			continue
		}
		copies = append(copies, c)
		seqs = append(seqs, c.seq)
	}
	slices.Sort(seqs)
	seqs = slices.Compact(seqs)
	if len(seqs) < e.tune.copiesKept {
		return nil
	}
	oldest := seqs[len(seqs)-e.tune.copiesKept]
	for _, c := range copies {
		if c.seq < oldest {
			if err = e.remove(ctx, c.key); err != nil {
				return err
			}
		}
	}
	names, err := e.expiredTrash(ctx, oldest)
	if err != nil {
		return err
	}
	// A crash before the commit only means deleting the same names again.
	// Deletes stop when the next copy is due, so a long backlog cannot hold
	// copies up; the rest go in later cleanups.
	for i, name := range names {
		if time.Now().After(deadline) {
			names = names[:i]
			break
		}
		if err = e.remove(ctx, chunkPrefix+name); err != nil {
			return err
		}
		if err = e.step(stepTrashDeleted); err != nil {
			return err
		}
	}
	return e.commit(ctx, func(tx *sql.Tx) error {
		for _, name := range names {
			if _, err := tx.ExecContext(ctx, `DELETE FROM trash WHERE name = ?`, name); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM copies WHERE seq < ?`, oldest)
		return err
	})
}

func (e *Engine) expiredTrash(ctx context.Context, oldest int64) (names []string, err error) {
	defer func() { err = e.diskError(err) }()
	rows, err := e.db.QueryContext(ctx, `SELECT t.name FROM trash t WHERE t.seq < ?
		AND NOT EXISTS (SELECT 1 FROM chunks c WHERE c.name = t.name)
		AND NOT EXISTS (SELECT 1 FROM pending p WHERE p.name = t.name)
		ORDER BY t.name`, oldest)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	for rows.Next() {
		var name string
		if err = rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, rows.Err()
}
