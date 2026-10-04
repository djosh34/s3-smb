package smbfs

import (
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/smb"
)

func TestMetadataQueriesDoNotWaitForColdRead(t *testing.T) {
	f := newFixture(t, 0)
	r := f.create(t, "data", smb.KindFile)
	h := f.open(t, r.Object, smb.AccessRead|smb.AccessWrite)
	write(t, f.fs, h, "cold", 0)
	if err := f.fs.Flush(t.Context(), h, smb.SyncData); err != nil {
		t.Fatal(err)
	}
	f.store.cold.Store(true)
	readDone := make(chan error, 1)
	go func() { _, err := f.fs.ReadAt(t.Context(), h, make([]byte, 4), 0); readDone <- err }()
	select {
	case <-f.store.started:
	case <-time.After(2 * time.Second):
		t.Fatal("cold GET not reached")
	}
	queriesDone := make(chan error, 1)
	go func() {
		_, err := f.fs.GetAttr(t.Context(), r.Object)
		if err == nil {
			_, err = f.fs.Lookup(t.Context(), "data")
		}
		if err == nil {
			_, err = f.fs.ReadDir(t.Context(), 1, 0, 10)
		}
		queriesDone <- err
	}()
	select {
	case err := <-queriesDone:
		if err != nil {
			t.Error(err)
		}
	case <-time.After(time.Second):
		t.Error("metadata query waited for cold read")
	}
	completed := false
	select {
	case err := <-readDone:
		completed = true
		t.Errorf("cold read completed before release: %v", err)
	default:
	}
	f.store.cold.Store(false)
	close(f.store.resume)
	if !completed {
		if err := <-readDone; err != nil {
			t.Fatal(err)
		}
	}
}

func TestIOUsesRetainedKindAndLength(t *testing.T) {
	f := newFixture(t, 0)
	r := f.create(t, "data", smb.KindFile)
	h := f.open(t, r.Object, smb.AccessRead|smb.AccessWrite)
	counted := &countedMetadata{Meta: f.metadata}
	f.fs.metadata = counted
	for i := 0; i < 5; i++ {
		write(t, f.fs, h, "data", 0)
		read(t, f.fs, h, []byte("data"))
	}
	if counted.attrs.Load() != 0 || counted.xattrs.Load() != 0 {
		t.Fatalf("adapter queried attributes during I/O: attr=%d, xattr=%d", counted.attrs.Load(), counted.xattrs.Load())
	}
}
