// SPDX-License-Identifier: AGPL-3.0-only
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

func TestEncryptedZstdFreshDatasetAndColdRecovery(t *testing.T) {
	f := newFixture(t, true)
	f.compression = "zstd"
	d := f.start()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	identity, err := f.store.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(f.bucket), Key: aws.String("s3-smb/format.json")})
	if err != nil {
		t.Fatal(err)
	}
	var format struct{ Compression, EncryptAlgo string }
	err = json.NewDecoder(identity.Body).Decode(&format)
	identity.Body.Close()
	if err != nil || format.Compression != "zstd" || format.EncryptAlgo == "" {
		t.Fatalf("fresh configured native format: %+v, %v", format, err)
	}
	data := bytes.Repeat([]byte("compressible native encrypted fixture\n"), 8192)
	files := map[string][]byte{"compressed.bin": data, "empty": {}}
	share, closeShare := f.share()
	if err = share.MkdirAll("nested/empty", 0700); err != nil {
		t.Fatal(err)
	}
	for name, payload := range files {
		writeFile(t, share, name, payload)
	}
	closeShare()
	f.protectedAfter(time.Now())
	d.stop()

	// Observe actual remote ciphertext, not a codec-only test or a capacity claim.
	objects, err := f.store.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(f.bucket)})
	if err != nil {
		t.Fatal(err)
	}
	chunks, backups := 0, 0
	for _, entry := range objects.Contents {
		key := aws.ToString(entry.Key)
		if !strings.Contains(key, "/chunks/") && !strings.Contains(key, "/meta/snapshot-") {
			continue
		}
		obj, e := f.store.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(f.bucket), Key: entry.Key})
		if e != nil {
			t.Fatal(e)
		}
		stored, e := io.ReadAll(obj.Body)
		obj.Body.Close()
		if e != nil {
			t.Fatal(e)
		}
		if strings.Contains(key, "/chunks/") {
			chunks++
			if len(stored) >= len(data)/2 || bytes.Contains(stored, []byte("compressible native encrypted fixture")) {
				t.Fatal("compressible fixture was not compressed before encryption")
			}
		} else {
			backups++
			if bytes.HasPrefix(stored, []byte{0x1f, 0x8b}) {
				t.Fatal("native metadata backup exposed plaintext gzip")
			}
		}
	}
	if chunks == 0 || backups == 0 {
		t.Fatal("missing actual native data or metadata objects")
	}
	// No old config/cache/database/key or compression selection reaches recovery.
	// Also omit the convenient identity: native backup inspection, not the
	// temporary uncompressed bootstrap candidate, must supply the saved codec.
	startupDelete(f, "s3-smb/format.json")
	f.freshLocal()
	f.compression = ""
	d = f.start()
	share, closeShare = f.share()
	verifyFiles(t, share, files)
	if info, e := share.Stat("nested/empty"); e != nil || !info.IsDir() {
		t.Fatalf("native metadata directory recovery: %v", e)
	}
	writeFile(t, share, "resumed.txt", []byte("write after compressed cold recovery"))
	closeShare()
	d.stop()
}

func TestCompressionSelectionCannotChangeExistingDataset(t *testing.T) {
	for _, stored := range []string{"none", "zstd"} {
		t.Run(stored, func(t *testing.T) {
			f := newFixture(t, true)
			if stored == "zstd" {
				f.compression = stored
			} // Old/default configuration omits the field and still creates none.
			d := f.start()
			share, closeShare := f.share()
			files := map[string][]byte{"preserved.txt": []byte("native existing format stays authoritative")}
			writeFile(t, share, "preserved.txt", files["preserved.txt"])
			closeShare()
			f.protectedAfter(time.Now())
			d.stop()
			before := startupRemoteSnapshot(f)
			f.freshLocal()
			f.compression = "zstd"
			if stored == "zstd" {
				f.compression = "none"
			}
			f.failStart = true
			f.start()
			assertStartupRemoteUnchanged(f, before)
			assertNoStartupDatabase(f)
			// Explicit matching assertions remain usable without format mutation.
			f.compression, f.failStart = stored, false
			d = f.start()
			share, closeShare = f.share()
			verifyFiles(t, share, files)
			closeShare()
			d.stop()
			if after := startupRemoteSnapshot(f); before["s3-smb/format.json"] != after["s3-smb/format.json"] {
				t.Fatal("matching selection rewrote stored format")
			}
		})
	}
}
