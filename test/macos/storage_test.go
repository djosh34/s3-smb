//go:build macos

// SPDX-License-Identifier: AGPL-3.0-only

package macos

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"github.com/djosh34/s3-smb/test/macos/helpers"
)

// storageSample is what the bucket held at one point of a run. Uploaded
// counts every chunk object listed so far in the run, so it misses only
// chunks deleted before the next listing.
type storageSample struct {
	At            time.Time `json:"at"`
	Label         string    `json:"label"`
	UploadedBytes int64     `json:"uploaded_bytes"`
	ChunkObjects  int       `json:"chunk_objects"`
	ChunkBytes    int64     `json:"chunk_bytes"`
	LiveBytes     int64     `json:"live_bytes"`
	TrashObjects  int       `json:"trash_objects"`
	TrashBytes    int64     `json:"trash_bytes"`
}

// storage lists the chunk objects, reads the live and trashed chunks from the
// local database and records the sizes. It only logs a failure, because it
// measures and does not judge.
func (h *harness) storage(label string) {
	objects, err := h.listObjects("chunks/")
	var tables chunkSets
	if err == nil {
		tables, err = h.chunkTables()
	}
	if err != nil {
		h.t.Log("storage-sample-failed", label, err)
		return
	}
	sample := storageSample{At: time.Now().UTC(), Label: label, ChunkObjects: len(objects), TrashObjects: len(tables.trash)}
	for _, size := range h.uploaded {
		sample.UploadedBytes += size
	}
	for _, size := range objects {
		sample.ChunkBytes += size
	}
	sample.LiveBytes = helpers.Bytes(objects, tables.live)
	sample.TrashBytes = helpers.Bytes(objects, tables.trash)
	h.samples = append(h.samples, sample)
	h.t.Logf("storage %s uploaded=%d chunks=%d/%d live=%d trash=%d/%d", label, sample.UploadedBytes, sample.ChunkObjects, sample.ChunkBytes, sample.LiveBytes, sample.TrashObjects, sample.TrashBytes)
}

// chunkSets holds chunk object keys, as in the bucket, from the local
// database. file maps each live chunk to its file.
type chunkSets struct {
	live, trash map[string]bool
	file        map[string]int64
}

// chunkTables reads the chunks and trash tables of s3-smb's database, read
// only and in one snapshot, while s3-smb may run.
func (h *harness) chunkTables() (sets chunkSets, err error) {
	path := filepath.Join(h.local, "state", "db")
	if _, err = os.Stat(path); err != nil {
		return sets, err
	}
	uri := url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro&_busy_timeout=10000"}
	db, err := sql.Open("sqlite3", uri.String())
	if err != nil {
		return sets, err
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	tx, err := db.BeginTx(h.ctx, nil)
	if err != nil {
		return sets, err
	}
	defer func() { err = errors.Join(err, tx.Rollback()) }()
	sets = chunkSets{live: map[string]bool{}, trash: map[string]bool{}, file: map[string]int64{}}
	if err = readNames(h.ctx, tx, "SELECT name, file FROM chunks", sets.live, sets.file); err != nil {
		return sets, err
	}
	return sets, readNames(h.ctx, tx, "SELECT name, 0 FROM trash", sets.trash, nil)
}

// readNames adds each row's chunk key to set, and its file to files when
// files is not nil.
func readNames(ctx context.Context, tx *sql.Tx, query string, set map[string]bool, files map[string]int64) (err error) {
	rows, err := tx.QueryContext(ctx, query)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	for rows.Next() {
		var name string
		var file int64
		if err = rows.Scan(&name, &file); err != nil {
			return err
		}
		set["chunks/"+name] = true
		if files != nil {
			files["chunks/"+name] = file
		}
	}
	return rows.Err()
}
