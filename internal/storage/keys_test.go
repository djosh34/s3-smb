// SPDX-License-Identifier: AGPL-3.0-only
package storage

import (
	"bytes"
	"context"
	"encoding/asn1"
	"encoding/pem"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/juicefs/pkg/object"
	"github.com/emmansun/gmsm/pkcs"
)

var fixtureOnce sync.Once
var fixtureKey []byte
var fixtureErr error

func protectedFixture(t *testing.T) []byte {
	t.Helper()
	fixtureOnce.Do(func() {
		start := time.Now()
		fixtureKey, fixtureErr = generateKey("synthetic-test-passphrase")
		t.Logf("RSA3072 generation + scrypt protect: %s, PEM %.3f kB, KDF memory %.3f MB", time.Since(start), float64(len(fixtureKey))/1000, float64(128*scryptN*8)/1e6)
	})
	if fixtureErr != nil {
		t.Fatal(fixtureErr)
	}
	return bytes.Clone(fixtureKey)
}
func TestProtectedKeyProfile(t *testing.T) {
	data := protectedFixture(t)
	start := time.Now()
	key, err := unlockKey(data, "synthetic-test-passphrase")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("native parser unlock: %s", time.Since(start))
	if key.N.BitLen() != 3072 {
		t.Fatal("wrong key size")
	}
	if _, err = unlockKey(data, "wrong"); err == nil {
		t.Fatal("accepted wrong passphrase")
	}
	for name, bad := range map[string][]byte{"oversized": bytes.Repeat([]byte{'x'}, maxKeyBytes+1), "empty": nil, "extra": append(bytes.Clone(data), data...), "plaintext": pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("bad")})} {
		t.Run(name, func(t *testing.T) {
			if _, err := unlockKey(bad, "synthetic-test-passphrase"); err == nil {
				t.Fatal("accepted malformed key")
			}
		})
	}
	block, _ := pem.Decode(data)
	var envelope encryptedKeyInfo
	if err = unmarshalExact(block.Bytes, &envelope); err != nil {
		t.Fatal(err)
	}
	envelope.Data[len(envelope.Data)-1] ^= 1
	tampered, _ := asn1.Marshal(envelope)
	if _, err = unlockKey(pem.EncodeToMemory(&pem.Block{Type: block.Type, Bytes: tampered}), "synthetic-test-passphrase"); err == nil {
		t.Fatal("accepted unauthenticated ciphertext")
	}
	var params pkcs.PBES2Params
	if err = unmarshalExact(envelope.Algorithm.Parameters.FullBytes, &params); err != nil {
		t.Fatal(err)
	}
	var kdf scryptParameters
	if err = unmarshalExact(params.KeyDerivationFunc.Parameters.FullBytes, &kdf); err != nil {
		t.Fatal(err)
	}
	for _, n := range []int{1 << 30, 0, -1, 32768} {
		kdf.N = n
		encoded, _ := asn1.Marshal(kdf)
		params.KeyDerivationFunc.Parameters = asn1.RawValue{FullBytes: encoded}
		encoded, _ = asn1.Marshal(params)
		envelope.Algorithm.Parameters = asn1.RawValue{FullBytes: encoded}
		encoded, _ = asn1.Marshal(envelope)
		bad := pem.EncodeToMemory(&pem.Block{Type: block.Type, Bytes: encoded})
		if err = validateKeyEnvelope(bad); err == nil {
			t.Fatalf("accepted unapproved cost %d", n)
		}
	}
}

// A fault-injected native memory store tests publication semantics only. MinIO
// acceptance is separate; this does not claim network/lost-response coverage.
type conditionalMemory struct {
	object.ObjectStorage
	mu   sync.Mutex
	puts int
	lose bool
}

// Native memory Get uses a nonstandard absence error; normalize only this test
// fixture via its native Head result. Production S3 has a modeled NoSuchKey.
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
	if _, err := s.Head(ctx, key); err == nil {
		return errors.New("precondition failed")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := s.Put(ctx, key, r); err != nil {
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
func TestBootstrapPublication(t *testing.T) {
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
func BenchmarkProtectedKeyUnlock(b *testing.B) {
	data, err := generateKey("synthetic-test-passphrase")
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err = unlockKey(data, "synthetic-test-passphrase"); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(128*scryptN*8)/1e6, "KDF-MB")
}

func TestIdentityAndRecoveryDiscovery(t *testing.T) {
	ctx := context.Background()
	raw := memory(t)
	f, err := NewFormat("s3-smb", true, 14)
	if err != nil {
		t.Fatal(err)
	}
	data := protectedFixture(t)
	if err = raw.Put(ctx, keyPrefix+f.UUID+".pem", bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	blob, err := OpenVolume(ctx, raw, f, "synthetic-test-passphrase", false)
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
		t.Fatal("identity retained connection authority")
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
	_, candidate, err := DiscoverRecoveryVolume(ctx, raw, true, "synthetic-test-passphrase")
	if err != nil {
		t.Fatal(err)
	}
	if candidate.UUID != f.UUID || candidate.EncryptKey != f.EncryptKey || raw.puts != before {
		t.Fatal("recovery changed identity or published state")
	}
	if err = raw.Delete(ctx, keyPrefix+f.UUID+".pem"); err != nil {
		t.Fatal(err)
	}
	if _, err = OpenVolume(ctx, raw, f, "synthetic-test-passphrase", true); err == nil {
		t.Fatal("regenerated existing volume missing key")
	}
	if raw.puts != before {
		t.Fatal("attempted replacement key")
	}
	if err = raw.Put(ctx, keyPrefix+f.UUID+".pem", strings.NewReader("corrupt key")); err != nil {
		t.Fatal(err)
	}
	if _, err = OpenVolume(ctx, raw, f, "synthetic-test-passphrase", true); err == nil {
		t.Fatal("accepted corrupt key")
	}
	if raw.puts != before {
		t.Fatal("replaced corrupt key")
	}
}

func TestVolumeKeyFailureAndMode(t *testing.T) {
	ctx := context.Background()
	raw := memory(t)
	f, err := NewFormat("test", true, 14)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = OpenVolume(ctx, raw, f, "synthetic-test-passphrase", false); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing key: %v", err)
	}
	if raw.puts != 0 {
		t.Fatal("recovery generated a key")
	}
	data := protectedFixture(t)
	if err = raw.Put(ctx, keyPrefix+f.UUID+".pem", bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	if _, err = OpenVolume(ctx, raw, f, "wrong", false); err == nil {
		t.Fatal("wrong passphrase accepted")
	}
	blob, err := OpenVolume(ctx, raw, f, "synthetic-test-passphrase", false)
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
		t.Fatal("plaintext remote content")
	}
	plaintext, err := readBounded(ctx, blob, "fixture", 1000)
	if err != nil || string(plaintext) != "private fixture content" {
		t.Fatal("native decrypt", err)
	}
	f.EncryptAlgo = ""
	if _, err = OpenVolume(ctx, raw, f, "", false); err == nil {
		t.Fatal("accepted inconsistent mode")
	}
	plain, err := NewFormat("plain", false, 14)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = OpenVolume(ctx, raw, plain, "", true); err != nil {
		t.Fatal(err)
	}
	if raw.puts != 0 {
		t.Fatal("plain mode/bootstrap recovery published key")
	}
}
