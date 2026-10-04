// SPDX-License-Identifier: AGPL-3.0-only
package storage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/djosh34/s3-smb/internal/juicefs/pkg/object"
)

// conditionalMemory adds PutIfAbsent to the JuiceFS memory store. It can lose
// the response of a committed PUT.
type conditionalMemory struct {
	object.ObjectStorage
	mu   sync.Mutex
	puts int
	lose bool
}

// Get reports a missing key as os.ErrNotExist, as the S3 client does.
func (s *conditionalMemory) Get(ctx context.Context, key string, off, limit int64, getters ...object.AttrGetter) (io.ReadCloser, error) {
	if _, err := s.Head(ctx, key); err != nil {
		return nil, err
	}
	return s.ObjectStorage.Get(ctx, key, off, limit, getters...)
}

func (s *conditionalMemory) PutIfAbsent(ctx context.Context, key string, r io.Reader) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.puts++
	_, err := s.Head(ctx, key)
	if err == nil {
		return errors.New("precondition failed")
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err = s.Put(ctx, key, r); err != nil {
		return err
	}
	if s.lose {
		return errors.New("lost response")
	}
	return nil
}

func memory(t *testing.T) *conditionalMemory {
	t.Helper()
	raw, err := object.CreateStorage("mem", "test", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	return &conditionalMemory{ObjectStorage: raw}
}

func TestPublishExact(t *testing.T) {
	ctx := context.Background()
	raw := memory(t)
	raw.lose = true
	if err := publishExact(ctx, raw, "key", []byte("original")); err != nil {
		t.Fatal(err)
	}
	if err := publishExact(ctx, raw, "key", []byte("replacement")); err == nil {
		t.Fatal("replaced existing key")
	}
	got, err := readBounded(ctx, raw, "key", 100)
	if err != nil || string(got) != "original" {
		t.Fatal("original was not preserved", err)
	}
	if err = publishExact(ctx, raw, "key", []byte("original")); err != nil {
		t.Fatal("identical retry", err)
	}
}

func TestIdentityAndRecoveryDiscovery(t *testing.T) {
	ctx := context.Background()
	raw := memory(t)
	f, err := NewFormat(VolumeName, true, 14)
	if err != nil {
		t.Fatal(err)
	}
	if err = raw.Put(ctx, keyPrefix+f.UUID+".pem", bytes.NewReader(protectedFixture(t))); err != nil {
		t.Fatal(err)
	}
	blob, err := OpenVolume(ctx, raw, f, testPassphrase, false)
	if err != nil {
		t.Fatal(err)
	}
	f.AccessKey = "must-not-save-access"
	f.SecretKey = "must-not-save-secret"
	f.SessionToken = "must-not-save-token"
	f.Bucket = "must-not-save-endpoint"
	if err = PublishIdentity(ctx, raw, f); err != nil {
		t.Fatal(err)
	}
	identity, err := readBounded(ctx, raw, identityKey, 128*1024)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(identity, []byte("must-not-save")) {
		t.Fatal("identity stored the bucket or credentials")
	}
	saved, err := ReadIdentity(ctx, raw)
	if err != nil || saved.UUID != f.UUID || saved.EncryptKey != f.EncryptKey {
		t.Fatal("identity round trip", err)
	}
	if err = PublishMarker(ctx, blob, f); err != nil {
		t.Fatal(err)
	}
	if err = VerifyMarker(ctx, blob, f); err != nil {
		t.Fatal(err)
	}
	if err = PublishMarker(ctx, blob, f); err != nil {
		t.Fatal("marker retry", err)
	}

	if err = raw.Delete(ctx, identityKey); err != nil {
		t.Fatal(err)
	}
	before := raw.puts
	_, candidate, err := DiscoverRecoveryVolume(ctx, raw, true, testPassphrase)
	if err != nil {
		t.Fatal(err)
	}
	if candidate.UUID != f.UUID || candidate.EncryptKey != f.EncryptKey || raw.puts != before {
		t.Fatal("recovery changed identity or published state")
	}

	// An existing volume never gets a new key, also when its key is missing or corrupt.
	if err = raw.Delete(ctx, keyPrefix+f.UUID+".pem"); err != nil {
		t.Fatal(err)
	}
	if _, err = OpenVolume(ctx, raw, f, testPassphrase, true); err == nil {
		t.Fatal("regenerated a missing key of an existing volume")
	}
	if err = raw.Put(ctx, keyPrefix+f.UUID+".pem", strings.NewReader("corrupt key")); err != nil {
		t.Fatal(err)
	}
	if _, err = OpenVolume(ctx, raw, f, testPassphrase, true); err == nil {
		t.Fatal("accepted corrupt key")
	}
	if raw.puts != before {
		t.Fatal("published a replacement key")
	}
}

func TestVolumeKeyFailureAndMode(t *testing.T) {
	ctx := context.Background()
	raw := memory(t)
	f, err := NewFormat("test", true, 14)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = OpenVolume(ctx, raw, f, testPassphrase, false); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing key: %v", err)
	}
	if raw.puts != 0 {
		t.Fatal("opening an existing volume generated a key")
	}
	if err = raw.Put(ctx, keyPrefix+f.UUID+".pem", bytes.NewReader(protectedFixture(t))); err != nil {
		t.Fatal(err)
	}
	if _, err = OpenVolume(ctx, raw, f, "wrong", false); err == nil {
		t.Fatal("wrong passphrase accepted")
	}
	blob, err := OpenVolume(ctx, raw, f, testPassphrase, false)
	if err != nil {
		t.Fatal(err)
	}
	if err = blob.Put(ctx, "fixture", strings.NewReader("private fixture content")); err != nil {
		t.Fatal(err)
	}
	ciphertext, err := readBounded(ctx, raw, "test/fixture", 1000)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(ciphertext, []byte("private fixture")) {
		t.Fatal("stored plaintext")
	}
	plaintext, err := readBounded(ctx, blob, "fixture", 1000)
	if err != nil || string(plaintext) != "private fixture content" {
		t.Fatal("decrypt", err)
	}
	f.EncryptAlgo = ""
	if _, err = OpenVolume(ctx, raw, f, "", false); err == nil {
		t.Fatal("accepted a key without an encryption mode")
	}
	plain, err := NewFormat("plain", false, 14)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = OpenVolume(ctx, raw, plain, "", true); err != nil {
		t.Fatal(err)
	}
	if raw.puts != 0 {
		t.Fatal("unencrypted volume published a key")
	}
}

func TestValidateFormatRejectsCompression(t *testing.T) {
	format, err := NewFormat(VolumeName, false, 14)
	if err != nil {
		t.Fatal(err)
	}
	if format.Compression != "none" {
		t.Fatalf("new format compression = %q", format.Compression)
	}
	format.Compression = "zstd"
	if err = validateFormat(format); err == nil {
		t.Fatal("accepted a compressed volume")
	}
}
