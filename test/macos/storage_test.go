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
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
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
	objects := h.objects("chunks/")
	tables, err := h.chunkTables()
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
// database.
type chunkSets struct {
	live, trash map[string]bool
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
	sets = chunkSets{live: map[string]bool{}, trash: map[string]bool{}}
	if err = readNames(h.ctx, tx, "SELECT name FROM chunks", sets.live); err != nil {
		return sets, err
	}
	return sets, readNames(h.ctx, tx, "SELECT name FROM trash", sets.trash)
}

func readNames(ctx context.Context, tx *sql.Tx, query string, set map[string]bool) (err error) {
	rows, err := tx.QueryContext(ctx, query)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	for rows.Next() {
		var name string
		if err = rows.Scan(&name); err != nil {
			return err
		}
		set["chunks/"+name] = true
	}
	return rows.Err()
}

// useB2 points s3-smb at the B2 test bucket that the workflow passes in. The
// keys stay in the environment.
func (h *harness) useB2() {
	for _, name := range []string{"B2_KEY_ID", "B2_APPLICATION_KEY", "B2_ENDPOINT", "B2_BUCKET"} {
		if os.Getenv(name) == "" {
			h.t.Fatal(name + " is required for the b2 scenario")
		}
	}
	region, err := helpers.B2Region(os.Getenv("B2_ENDPOINT"))
	h.must(err)
	h.b2, h.endpoint, h.bucket, h.region = true, os.Getenv("B2_ENDPOINT"), os.Getenv("B2_BUCKET"), region
}

func b2Client(region string) *s3.Client {
	return s3.New(s3.Options{
		Region:       region,
		BaseEndpoint: aws.String(os.Getenv("B2_ENDPOINT")),
		UsePathStyle: true,
		Credentials:  credentials.NewStaticCredentialsProvider(os.Getenv("B2_KEY_ID"), os.Getenv("B2_APPLICATION_KEY"), ""),
	})
}

// connectB2 connects to the B2 test bucket, which must be empty.
func (h *harness) connectB2() {
	h.s3 = b2Client(h.region)
	if len(h.objects("")) != 0 {
		h.t.Fatal("the B2 test bucket is not empty")
	}
}

// TestEmptyB2Bucket deletes every object version in the B2 test bucket by ID
// and fails if any is left. The b2 job runs it after the backup, also when
// that failed.
func TestEmptyB2Bucket(t *testing.T) {
	region, err := helpers.B2Region(os.Getenv("B2_ENDPOINT"))
	if err != nil {
		t.Fatal(err)
	}
	client, bucket := b2Client(region), os.Getenv("B2_BUCKET")
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Minute)
	defer cancel()
	for range 3 {
		var left int
		pages := s3.NewListObjectVersionsPaginator(client, &s3.ListObjectVersionsInput{Bucket: aws.String(bucket)})
		for pages.HasMorePages() {
			page, err := pages.NextPage(ctx)
			if err != nil {
				t.Fatal(err)
			}
			var ids [][2]*string
			for _, v := range page.Versions {
				ids = append(ids, [2]*string{v.Key, v.VersionId})
			}
			for _, m := range page.DeleteMarkers {
				ids = append(ids, [2]*string{m.Key, m.VersionId})
			}
			left += len(ids)
			for _, id := range ids {
				if _, err = client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: id[0], VersionId: id[1]}); err != nil {
					t.Error(err)
				}
			}
		}
		t.Log("deleted object versions", left)
		if left == 0 {
			return
		}
	}
	t.Error("object versions are left in the B2 test bucket")
}
