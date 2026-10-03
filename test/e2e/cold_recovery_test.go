// SPDX-License-Identifier: AGPL-3.0-only
package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/backup"
	smb "github.com/hirochachacha/go-smb2"
)

func (f *fixture) receipt() backup.Receipt {
	f.t.Helper()
	data, err := os.ReadFile(filepath.Join(f.root, "state", "backup-receipt.json"))
	if err != nil {
		f.t.Fatal(err)
	}
	var receipt backup.Receipt
	if err = json.Unmarshal(data, &receipt); err != nil {
		f.t.Fatal(err)
	}
	return receipt
}

// An interrupted Time Machine backup leaves chunk objects in S3 that the
// newest metadata backup does not describe, or describes half-written. Cold
// recovery from that metadata backup must return the earlier files unchanged.
func TestColdRecoveryAfterInterruptedWrites(t *testing.T) {
	// The pattern differs from every file in crashBaseline.
	later := make([]byte, 24_000_000)
	for i := range later {
		later[i] = byte((i*7 + i/97) % 253)
	}
	t.Run("backup before the writes", func(t *testing.T) {
		f := newFixture(t, true)
		// The writes after the backup and the kill take less than one interval.
		f.interval = "20s"
		d := f.start()
		s, closeShare := f.share()
		files := crashBaseline()
		for name, data := range files {
			writeFile(t, s, name, data)
		}
		closeShare()
		f.protectedAfter(time.Now())
		before := f.receipt()
		s, closeShare = f.share()
		writeFile(t, s, "after-backup.bin", later)
		overwrite(t, s, "baseline-large.bin", later[:300_000])
		sigkill(t, d)
		closeShare()
		if at := f.receipt(); at.Key != before.Key {
			t.Fatalf("a metadata backup landed between the writes and the kill: %s then %s", before.Key, at.Key)
		}
		recoverTwice(t, f, files)
	})
	t.Run("backup during a large write", func(t *testing.T) {
		f := newFixture(t, true)
		d := f.start()
		s, closeShare := f.share()
		files := crashBaseline()
		for name, data := range files {
			writeFile(t, s, name, data)
		}
		closeShare()
		f.protectedAfter(time.Now())
		s, closeShare = f.share()
		// The next metadata backup holds this overwrite, so a recovery from the
		// one before it fails the hash check.
		files["baseline-large.bin"] = overwrite(t, s, "baseline-large.bin", later[:300_000])
		file, err := s.Create("unfinished.bin")
		if err != nil {
			t.Fatal(err)
		}
		// The writer runs until the kill closes its connection.
		first, stopped := make(chan error, 1), make(chan struct{})
		var writeErr error
		go func() {
			defer close(stopped)
			_, writeErr = file.Write(later)
			first <- writeErr
			for writeErr == nil {
				_, writeErr = file.Write(later)
			}
		}()
		if err = <-first; err != nil {
			t.Fatal(err)
		}
		f.protectedAfter(time.Now())
		select {
		case <-stopped:
			t.Fatalf("the large write ended before the metadata backup landed: %v", writeErr)
		default:
		}
		sigkill(t, d)
		<-stopped
		closeShare()
		recoverTwice(t, f, files)
	})
}

// recoverTwice recovers from S3 with no local state, checks the files, writes a
// new one, and does the same again from the next metadata backup.
func recoverTwice(t *testing.T, f *fixture, files map[string][]byte) {
	t.Helper()
	f.interval = ""
	f.freshLocal()
	d := f.start()
	s, closeShare := f.share()
	verifyFiles(t, s, files)
	files["after-recovery.txt"] = []byte("written on top of the recovered state\n")
	writeFile(t, s, "after-recovery.txt", files["after-recovery.txt"])
	closeShare()
	f.protectedAfter(time.Now())
	d.stop()
	f.freshLocal()
	d = f.start()
	s, closeShare = f.share()
	verifyFiles(t, s, files)
	closeShare()
	d.stop()
}

// overwrite writes data at offset 200,000 of an existing file, syncs it and
// returns the new contents.
func overwrite(t *testing.T, share *smb.Share, name string, data []byte) []byte {
	t.Helper()
	contents, err := share.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	copy(contents[200_000:], data)
	file, err := share.OpenFile(name, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = file.WriteAt(data, 200_000); err != nil {
		t.Fatal(err)
	}
	if err = file.Sync(); err != nil {
		t.Fatal(err)
	}
	if err = file.Close(); err != nil {
		t.Fatal(err)
	}
	return contents
}
