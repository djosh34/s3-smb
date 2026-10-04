// SPDX-License-Identifier: AGPL-3.0-only
package e2e

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/djosh34/s3-smb/internal/backup"
	"github.com/djosh34/s3-smb/internal/config"
	"github.com/djosh34/s3-smb/internal/juicefs/pkg/meta"
	"github.com/djosh34/s3-smb/internal/storage"
	"golang.org/x/sys/unix"
)

// Remote snapshots include all keys, full content hashes, ETags and modification
// times, so a rejected startup cannot hide object replacement behind equal data.
type remoteFingerprint struct {
	Hash     [32]byte
	ETag     string
	Modified time.Time
}

func startupRemoteSnapshot(f *fixture) map[string]remoteFingerprint {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	result := make(map[string]remoteFingerprint)
	pages := s3.NewListObjectsV2Paginator(f.store, &s3.ListObjectsV2Input{Bucket: aws.String(f.bucket)})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			f.t.Fatal(err)
		}
		for _, entry := range page.Contents {
			obj, err := f.store.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(f.bucket), Key: entry.Key})
			if err != nil {
				f.t.Fatal(err)
			}
			data, err := io.ReadAll(obj.Body)
			closeErr := obj.Body.Close()
			if err != nil || closeErr != nil {
				f.t.Fatalf("snapshot read: %v %v", err, closeErr)
			}
			result[aws.ToString(entry.Key)] = remoteFingerprint{Hash: sha256.Sum256(data), ETag: aws.ToString(entry.ETag), Modified: aws.ToTime(entry.LastModified)}
		}
	}
	return result
}
func startupPut(f *fixture, key string, data []byte) {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := f.store.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(f.bucket), Key: aws.String(key), Body: bytes.NewReader(data)}); err != nil {
		f.t.Fatal(err)
	}
}
func startupDelete(f *fixture, key string) {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := f.store.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(f.bucket), Key: aws.String(key)}); err != nil {
		f.t.Fatal(err)
	}
}
func startupKeys(objects map[string]remoteFingerprint, prefix string) []string {
	var keys []string
	for key := range objects {
		if strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	return keys
}
func assertStartupRemoteUnchanged(f *fixture, before map[string]remoteFingerprint) {
	f.t.Helper()
	after := startupRemoteSnapshot(f)
	if !reflect.DeepEqual(before, after) {
		f.t.Fatalf("startup mutated remote objects: before=%v after=%v", before, after)
	}
}
func assertNoStartupDatabase(f *fixture) {
	f.t.Helper()
	if _, err := os.Lstat(filepath.Join(f.root, "state", "metadata.db")); !os.IsNotExist(err) {
		f.t.Fatalf("rejected startup created local metadata: %v", err)
	}
}

func TestStartupRejectsCompressedFormat(t *testing.T) {
	f := newFixture(t, false)
	format, err := storage.NewFormat(storage.VolumeName, false, 14)
	if err != nil {
		t.Fatal(err)
	}
	format.Compression = "zstd"
	data, err := json.Marshal(format)
	if err != nil {
		t.Fatal(err)
	}
	startupPut(f, "s3-smb/format.json", data)
	before := startupRemoteSnapshot(f)
	f.failStart = true
	d := f.start()
	assertNoStartupDatabase(f)
	assertStartupRemoteUnchanged(f, before)
	stderr, err := os.ReadFile(d.logs[1].Name())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(stderr, []byte("unsupported compression")) {
		t.Fatal("startup did not reject the stored compression format")
	}
}

func TestStartupRejectsPartialRemoteState(t *testing.T) {
	for _, tc := range []struct {
		name, key           string
		encrypted, readonly bool
	}{
		{name: "unknown-object", key: "unrelated-owner/important"},
		{name: "data-only", key: "s3-smb/chunks/0/0/1_0_64"},
		{name: "marker-only", key: "s3-smb/juicefs_uuid"},
		{name: "key-only", key: "s3-smb/keys/3be3c62a-942e-4c82-8d4c-d196a54d8a02.pem", encrypted: true},
		{name: "partial-format", key: "s3-smb/format.json"},
		{name: "readonly-empty", readonly: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, tc.encrypted)
			f.readonly = tc.readonly
			if tc.key != "" {
				startupPut(f, tc.key, []byte("existing partial state must stay untouched"))
			}
			before := startupRemoteSnapshot(f)
			f.failStart = true
			f.start()
			assertStartupRemoteUnchanged(f, before)
			assertNoStartupDatabase(f)
		})
	}
}

// For credential/listing failures no confirmation should be attempted. Use a
// detached actual executable with piped yes: it must report listing failure, not
// classify an empty bucket and merely fail for lack of /dev/tty.
func startupWithBadListingConfig(f *fixture, text string) {
	f.t.Helper()
	f.generation++
	path := filepath.Join(f.root, "bad-listing-config.yaml")
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		f.t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Getenv("S3_SMB_E2E_BINARY"), "-c", path, "serve")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdin = strings.NewReader("yes\n")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	artifacts := os.Getenv("S3_SMB_TEST_ARTIFACTS")
	if artifacts == "" {
		artifacts = f.t.TempDir()
	}
	dir := filepath.Join(artifacts, strings.ReplaceAll(f.t.Name(), "/", "-"), fmt.Sprint(f.generation))
	if e := os.MkdirAll(dir, 0700); e != nil {
		f.t.Fatal(e)
	}
	for name, data := range map[string][]byte{"stdout.log": stdout.Bytes(), "stderr.log": stderr.Bytes()} {
		out := filepath.Join(dir, name)
		if e := os.WriteFile(out, data, 0600); e != nil {
			f.t.Fatal(e)
		}
		checkDaemonLog(f.t, out)
	}
	if err == nil || ctx.Err() != nil {
		f.t.Fatalf("listing failure did not exit promptly/nonzero: %v %v", err, ctx.Err())
	}
	if !bytes.Contains(stderr.Bytes(), []byte("remote dataset listing failed; refusing initialization")) {
		f.t.Fatalf("did not distinguish listing error from empty/TTY failure; artifacts %s", dir)
	}
	if bytes.Contains(stderr.Bytes(), []byte("s3smb-wrong-secret-marker")) {
		f.t.Fatal("wrong credential leaked")
	}
}
func TestStartupListingFailuresAreNotEmpty(t *testing.T) {
	for _, mode := range []string{"wrong-secret", "missing-bucket"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t, false)
			before := startupRemoteSnapshot(f)
			text := f.config()
			if mode == "wrong-secret" {
				text = strings.Replace(text, "s3smb-test-secret-only", "s3smb-wrong-secret-marker", 1)
			} else {
				text = strings.Replace(text, fmt.Sprintf("bucket: %q", f.bucket), fmt.Sprintf("bucket: %q", f.bucket+"-absent"), 1)
			}
			startupWithBadListingConfig(f, text)
			assertStartupRemoteUnchanged(f, before)
			assertNoStartupDatabase(f)
		})
	}
}

func startupProtectedFixture(t *testing.T, encrypted bool) (*fixture, map[string][]byte) {
	t.Helper()
	f := newFixture(t, encrypted)
	d := f.start()
	share, closeShare := f.share()
	files := map[string][]byte{"preserved.txt": []byte("every recovery fixture must survive\n"), "empty": {}}
	for name, data := range files {
		writeFile(t, share, name, data)
	}
	closeShare()
	f.protectedAfter(time.Now())
	d.stop()
	if points := startupKeys(startupRemoteSnapshot(f), "s3-smb/meta/snapshot-"); len(points) < 2 {
		t.Fatal("fixture needs an older valid point plus a newer protected point")
	}
	return f, files
}
func TestStartupRejectsBrokenRecovery(t *testing.T) {
	for _, encrypted := range []bool{false, true} {
		mode := "plaintext"
		if encrypted {
			mode = "encrypted"
		}
		cases := []string{"corrupt-latest-backup", "missing-selected-backup", "missing-all-backups", "mode-mismatch"}
		if encrypted {
			cases = append(cases, "wrong-passphrase", "missing-key", "corrupt-key", "wrong-key")
		}
		for _, fault := range cases {
			t.Run(mode+"/"+fault, func(t *testing.T) {
				f, _ := startupProtectedFixture(t, encrypted)
				objects := startupRemoteSnapshot(f)
				points := startupKeys(objects, "s3-smb/meta/snapshot-")
				if fault == "missing-selected-backup" {
					f.freshLocal()
					before := startupLoseSelectedPoint(f, points[len(points)-1])
					assertStartupRemoteUnchanged(f, before)
					assertNoStartupDatabase(f)
					return
				}
				switch fault {
				case "corrupt-latest-backup":
					startupPut(f, points[len(points)-1], []byte("corrupt latest checkpoint; valid older checkpoint remains"))
				case "missing-all-backups":
					for _, key := range points {
						startupDelete(f, key)
					}
				case "mode-mismatch":
					f.encrypted = !f.encrypted
				case "wrong-passphrase":
					f.secret = "wrong-encryption-passphrase-e2e-marker"
				case "missing-key", "corrupt-key", "wrong-key":
					keys := startupKeys(objects, "s3-smb/keys/")
					if len(keys) != 1 {
						t.Fatalf("expected one native bootstrap key, found %d", len(keys))
					}
					if fault == "missing-key" {
						startupDelete(f, keys[0])
					} else if fault == "corrupt-key" {
						startupPut(f, keys[0], []byte("corrupt protected key must not be regenerated"))
					} else {
						other := newFixture(t, true)
						// Keep the second volume's logs separate from f's generations.
						other.generation = 1000
						d := other.start()
						d.stop()
						otherKeys := startupKeys(startupRemoteSnapshot(other), "s3-smb/keys/")
						if len(otherKeys) != 1 {
							t.Fatal("second volume must have exactly one bootstrap key")
						}
						ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
						defer cancel()
						obj, err := other.store.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(other.bucket), Key: aws.String(otherKeys[0])})
						if err != nil {
							t.Fatal(err)
						}
						data, err := io.ReadAll(obj.Body)
						obj.Body.Close()
						if err != nil {
							t.Fatal(err)
						}
						// Valid encrypted PKCS8 key, same passphrase, different volume key.
						startupPut(f, keys[0], data)
					}
				}
				before := startupRemoteSnapshot(f)
				f.freshLocal()
				f.failStart = true
				f.start()
				assertStartupRemoteUnchanged(f, before)
				assertNoStartupDatabase(f)
			})
		}
	}
}

// Remove the already selected newest backup only after the real recovery prompt
// displays it. An older valid point remains, but the process must not fall back.
func startupLoseSelectedPoint(f *fixture, key string) map[string]remoteFingerprint {
	f.t.Helper()
	f.generation++
	path := filepath.Join(f.root, "config.yaml")
	if err := os.WriteFile(path, []byte(f.config()), 0600); err != nil {
		f.t.Fatal(err)
	}
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		f.t.Fatal(err)
	}
	defer master.Close()
	if err = unix.IoctlSetPointerInt(int(master.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		f.t.Fatal(err)
	}
	number, err := unix.IoctlGetInt(int(master.Fd()), unix.TIOCGPTN)
	if err != nil {
		f.t.Fatal(err)
	}
	slave, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", number), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		f.t.Fatal(err)
	}
	defer slave.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Getenv("S3_SMB_E2E_BINARY"), "serve", "-c", path)
	cmd.Stdin = slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err = cmd.Start(); err != nil {
		f.t.Fatal(err)
	}
	slave.Close()
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	var prompt strings.Builder
	for !strings.Contains(prompt.String(), "Continue? [yes/no]:") {
		var buf [1024]byte
		n, e := master.Read(buf[:])
		if e != nil {
			cmd.Process.Kill()
			cmd.Wait()
			f.t.Fatalf("no recovery prompt: %v; stderr %s", e, stderr.String())
		}
		prompt.Write(buf[:n])
		if prompt.Len() > 8192 {
			f.t.Fatal("unexpected oversized prompt")
		}
	}
	if !strings.Contains(prompt.String(), strings.TrimPrefix(key, "s3-smb/")) {
		f.t.Fatal("prompt did not select the newest expected point")
	}
	startupDelete(f, key)
	before := startupRemoteSnapshot(f)
	if _, err = master.WriteString("yes\n"); err != nil {
		f.t.Fatal(err)
	}
	err = cmd.Wait()
	artifacts := os.Getenv("S3_SMB_TEST_ARTIFACTS")
	if artifacts == "" {
		artifacts = f.t.TempDir()
	}
	dir := filepath.Join(artifacts, strings.ReplaceAll(f.t.Name(), "/", "-"), fmt.Sprint(f.generation))
	if e := os.MkdirAll(dir, 0700); e != nil {
		f.t.Fatal(e)
	}
	for name, data := range map[string][]byte{"stdout.log": stdout.Bytes(), "stderr.log": stderr.Bytes(), "prompts.log": []byte(prompt.String())} {
		out := filepath.Join(dir, name)
		if e := os.WriteFile(out, data, 0600); e != nil {
			f.t.Fatal(e)
		}
		if name != "prompts.log" {
			checkDaemonLog(f.t, out)
		}
	}
	if err == nil || ctx.Err() != nil {
		f.t.Fatalf("missing selected backup did not promptly fail: %v %v", err, ctx.Err())
	}
	if bytes.Contains(stderr.Bytes(), []byte("SMB serving")) || !bytes.Contains(stderr.Bytes(), []byte("recover selected metadata")) {
		f.t.Fatalf("wrong failure or writable fallback; artifacts %s", dir)
	}
	return before
}

// Poison only the selected snapshot's connection fields, using the real native
// decrypt/encrypt wrapper. Recovery must retain
// the new YAML's destination/credential authority in both encryption modes.
func startupPoisonSavedConnection(f *fixture) {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cfg, err := config.Load(filepath.Join(f.root, "config.yaml"))
	if err != nil {
		f.t.Fatal(err)
	}
	resolved, err := cfg.Resolve(ctx, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		f.t.Fatal(err)
	}
	raw, err := storage.OpenS3(resolved)
	if err != nil {
		f.t.Fatal(err)
	}
	if closer, ok := raw.(io.Closer); ok {
		defer func() {
			if err := closer.Close(); err != nil {
				f.t.Error(err)
			}
		}()
	}
	format, err := storage.ReadIdentity(ctx, raw)
	if err != nil {
		f.t.Fatal(err)
	}
	blob, err := storage.OpenVolume(ctx, raw, format, resolved.Passphrase, false)
	if err != nil {
		f.t.Fatal(err)
	}
	points, err := backup.List(ctx, blob)
	if err != nil || len(points) == 0 {
		f.t.Fatalf("native backup list: %v", err)
	}
	reader, err := blob.Get(ctx, points[0].Key, 0, -1)
	if err != nil {
		f.t.Fatal(err)
	}
	gz, err := gzip.NewReader(reader)
	if err != nil {
		f.t.Fatal(errors.Join(err, reader.Close()))
	}
	database, err := io.ReadAll(gz)
	if err = errors.Join(err, gz.Close(), reader.Close()); err != nil {
		f.t.Fatal(err)
	}
	path := filepath.Join(f.t.TempDir(), "snapshot.db")
	if err = os.WriteFile(path, database, 0600); err != nil {
		f.t.Fatal(err)
	}
	if err = startupPoisonSnapshotFormat(ctx, path); err != nil {
		f.t.Fatal(err)
	}
	database, err = os.ReadFile(path)
	if err != nil {
		f.t.Fatal(err)
	}
	hash := sha256.Sum256(database)
	var data bytes.Buffer
	writer := gzip.NewWriter(&data)
	writer.Comment = "sha256:" + hex.EncodeToString(hash[:])
	_, err = writer.Write(database)
	if err = errors.Join(err, writer.Close()); err != nil {
		f.t.Fatal(err)
	}
	// Intentional fixture corruption; production never overwrites backup names.
	if err = blob.Put(ctx, points[0].Key, bytes.NewReader(data.Bytes())); err != nil {
		f.t.Fatal(err)
	}
	if _, err = backup.Inspect(ctx, blob, points[0].Key, resolved.Storage.StateDir); err != nil {
		f.t.Fatalf("poisoned snapshot is not valid metadata: %v", err)
	}
}

func startupPoisonSnapshotFormat(ctx context.Context, path string) (err error) {
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	db.SetMaxOpenConns(1)
	if _, err = db.ExecContext(ctx, "PRAGMA journal_mode=DELETE"); err != nil {
		return err
	}
	var encoded string
	if err = db.QueryRowContext(ctx, "SELECT value FROM jfs_setting WHERE name='format'").Scan(&encoded); err != nil {
		return err
	}
	var format meta.Format
	if err = json.Unmarshal([]byte(encoded), &format); err != nil {
		return err
	}
	format.Bucket = "http://old-snapshot-destination.invalid:1/old-bucket"
	format.AccessKey = "old-snapshot-access-key"
	format.SecretKey = "old-snapshot-secret-key"
	format.SessionToken = "old-snapshot-token"
	data, err := json.Marshal(&format)
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, "UPDATE jfs_setting SET value=? WHERE name='format'", string(data))
	return err
}

func TestIdentityMissingRecoveryUsesCurrentConfig(t *testing.T) {
	for _, encrypted := range []bool{false, true} {
		name := "plaintext"
		if encrypted {
			name = "encrypted"
		}
		t.Run(name, func(t *testing.T) {
			f, files := startupProtectedFixture(t, encrypted)
			startupPoisonSavedConnection(f)
			startupDelete(f, "s3-smb/format.json")
			startupDelete(f, "s3-smb/juicefs_uuid")
			f.freshLocal()
			d := f.start()
			share, closeShare := f.share()
			verifyFiles(t, share, files)
			writeFile(t, share, "resumed.txt", []byte("writes with current connection config"))
			closeShare()
			d.stop()
			for _, log := range d.logs {
				data, err := os.ReadFile(log.Name())
				if err != nil {
					t.Fatal(err)
				}
				for _, secret := range []string{"old-snapshot-access-key", "old-snapshot-secret-key", "old-snapshot-token"} {
					if bytes.Contains(data, []byte(secret)) {
						t.Fatalf("old snapshot credential leaked in %s", log.Name())
					}
				}
			}
		})
	}
}
func TestReadOnlyColdRecoveryDoesNotMutateS3(t *testing.T) {
	for _, encrypted := range []bool{false, true} {
		name := "plaintext"
		if encrypted {
			name = "encrypted"
		}
		t.Run(name, func(t *testing.T) {
			f, files := startupProtectedFixture(t, encrypted)
			before := startupRemoteSnapshot(f)
			f.freshLocal()
			f.readonly = true
			d := f.start()
			share, closeShare := f.share()
			verifyFiles(t, share, files)
			if file, err := share.Create("forbidden.txt"); err == nil {
				file.Close()
				t.Fatal("read-only recovered volume accepted CREATE")
			}
			closeShare()
			// Exceed the configured native backup interval: readonly must not schedule it.
			time.Sleep(2500 * time.Millisecond)
			d.stop()
			assertStartupRemoteUnchanged(f, before)
			if _, err := os.Stat(filepath.Join(f.root, "state", "backup-receipt.json")); !os.IsNotExist(err) {
				t.Fatalf("read-only recovery created a backup receipt: %v", err)
			}
		})
	}
}
