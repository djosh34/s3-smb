// SPDX-License-Identifier: AGPL-3.0-only
package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/backup"
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
		old, err := s.OpenFile("baseline-large.bin", os.O_RDWR, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = old.WriteAt(later[:300_000], 200_000); err != nil {
			t.Fatal(err)
		}
		if err = old.Sync(); err != nil {
			t.Fatal(err)
		}
		if err = old.Close(); err != nil {
			t.Fatal(err)
		}
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
		file, err := s.Create("unfinished.bin")
		if err != nil {
			t.Fatal(err)
		}
		// The writer runs until the kill closes its connection.
		first, stopped := make(chan error, 1), make(chan struct{})
		go func() {
			defer close(stopped)
			_, err := file.Write(later)
			first <- err
			for err == nil {
				_, err = file.Write(later)
			}
		}()
		if err = <-first; err != nil {
			t.Fatal(err)
		}
		f.protectedAfter(time.Now())
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
