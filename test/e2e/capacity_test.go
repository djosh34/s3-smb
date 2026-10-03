// SPDX-License-Identifier: AGPL-3.0-only
package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/djosh34/s3-smb/internal/juicefs/pkg/meta"
	"github.com/djosh34/s3-smb/internal/storage"
)

func assertLocalCapacity(t *testing.T, f *fixture, want uint64) {
	t.Helper()
	conf := meta.DefaultConf()
	conf.ReadOnly = true
	conf.NoBGJob = true
	m, err := storage.OpenMetadata(filepath.Join(f.root, "state", "metadata.db"), conf)
	if err != nil {
		t.Fatal(err)
	}
	format, loadErr := m.Load(true)
	closeErr := m.Shutdown()
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	if format.Capacity != want {
		t.Fatalf("stored capacity: got %d, want %d", format.Capacity, want)
	}
}

func TestStorageCapacityAtInitializationAndRestart(t *testing.T) {
	for _, initial := range []string{"", "100 MB"} {
		t.Run(fmt.Sprintf("initial=%s", initial), func(t *testing.T) {
			f := newFixture(t, false)
			f.storageCapacity = initial
			d := f.start()
			d.stop()
			want := uint64(0)
			if initial != "" {
				want = 100000000
			}
			assertLocalCapacity(t, f, want)
			// The published identity also carries the initial capacity.
			out, err := f.store.GetObject(context.Background(), &s3.GetObjectInput{Bucket: aws.String(f.bucket), Key: aws.String("s3-smb/format.json")})
			if err != nil {
				t.Fatal(err)
			}
			data, readErr := io.ReadAll(out.Body)
			closeErr := out.Body.Close()
			if readErr != nil {
				t.Fatal(readErr)
			}
			if closeErr != nil {
				t.Fatal(closeErr)
			}
			var format meta.Format
			if err = json.Unmarshal(data, &format); err != nil {
				t.Fatal(err)
			}
			if format.Capacity != want {
				t.Fatalf("initial remote capacity: got %d, want %d", format.Capacity, want)
			}
			for _, next := range []struct {
				setting string
				bytes   uint64
			}{{"200 MB", 200000000}, {"10 MB", 10000000}, {"0", 0}, {"50 MB", 50000000}, {"", 0}} {
				f.storageCapacity = next.setting
				d = f.start()
				d.stop()
				assertLocalCapacity(t, f, next.bytes)
			}
		})
	}
}

func TestStorageCapacityOnRecovery(t *testing.T) {
	f := newFixture(t, false)
	f.storageCapacity = "100 MB"
	d := f.start()
	d.stop()
	f.freshLocal()
	f.storageCapacity = "200 MB"
	d = f.start()
	d.stop()
	assertLocalCapacity(t, f, 200000000)
}
