// SPDX-License-Identifier: AGPL-3.0-only

package e2e

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// TestRecovery recovers twice from S3 with no local state and checks that
// metadata backups are gzip only when encryption is off.
func TestRecovery(t *testing.T) {
	for _, encrypted := range []bool{false, true} {
		name := "plaintext"
		if encrypted {
			name = "encrypted"
		}
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, encrypted)
			d := f.start()
			share, disconnect := f.share()
			large := make([]byte, 5_000_003)
			for i := range large {
				large[i] = byte((i*31 + i/257) % 251)
			}
			files := map[string][]byte{"empty": {}, "unicode-文件.txt": []byte("all expected files are checked\n"), "large.bin": large}
			for name, data := range files {
				writeFile(t, share, name, data)
			}
			verifyFiles(t, share, files)
			disconnect()
			f.protectedAfter(time.Now())
			d.stop()
			f.freshLocal()
			d = f.start()
			share, disconnect = f.share()
			verifyFiles(t, share, files)
			files["resumed.txt"] = []byte("writes after fresh-install recovery\n")
			writeFile(t, share, "resumed.txt", files["resumed.txt"])
			disconnect()
			f.protectedAfter(time.Now())
			d.stop()
			f.freshLocal()
			d = f.start()
			share, disconnect = f.share()
			verifyFiles(t, share, files)
			disconnect()
			d.stop()
			checkSnapshotEncoding(t, f)
		})
	}
}

func checkSnapshotEncoding(t *testing.T, f *fixture) {
	t.Helper()
	objects, err := f.store.ListObjectsV2(t.Context(), &s3.ListObjectsV2Input{Bucket: aws.String(f.bucket)})
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, object := range objects.Contents {
		if !strings.Contains(aws.ToString(object.Key), "meta/snapshot-") {
			continue
		}
		count++
		body, err := f.store.GetObject(t.Context(), &s3.GetObjectInput{Bucket: aws.String(f.bucket), Key: object.Key})
		if err != nil {
			t.Fatal(err)
		}
		header := make([]byte, 2)
		_, err = io.ReadFull(body.Body, header)
		if err = errors.Join(err, body.Body.Close()); err != nil {
			t.Fatal(err)
		}
		if gzip := bytes.Equal(header, []byte{0x1f, 0x8b}); gzip == f.encrypted {
			t.Fatalf("metadata backup %s: gzip header %t with encryption %t", aws.ToString(object.Key), gzip, f.encrypted)
		}
	}
	if count == 0 {
		t.Fatal("no metadata backups found")
	}
}

func TestAuthentication(t *testing.T) {
	f := newFixture(t, false)
	d := f.start()
	share, disconnect := f.share()
	writeFile(t, share, "signed.txt", []byte("signed session"))
	disconnect()
	for _, credentials := range [][2]string{{"backup", "wrong-password"}, {"wrong-user", f.password}} {
		if _, disconnect, err := f.connect(credentials[0], credentials[1]); err == nil {
			disconnect()
			t.Errorf("user %q with password %q authenticated", credentials[0], credentials[1])
		}
	}
	d.stop()
}

// TestReadOnly restarts a written volume read-only and checks that it serves
// the files and refuses changes.
func TestReadOnly(t *testing.T) {
	f := newFixture(t, false)
	d := f.start()
	share, disconnect := f.share()
	data := []byte("read-only fixture")
	writeFile(t, share, "keep.txt", data)
	disconnect()
	d.stop()
	f.readonly = true
	d = f.start()
	share, disconnect = f.share()
	verifyFiles(t, share, map[string][]byte{"keep.txt": data})
	if file, err := share.Create("forbidden.txt"); err == nil {
		t.Error(errors.Join(errors.New("read-only CREATE succeeded"), file.Close()))
	}
	if err := share.Remove("keep.txt"); err == nil {
		t.Error("read-only DELETE succeeded")
	}
	disconnect()
	d.stop()
}
