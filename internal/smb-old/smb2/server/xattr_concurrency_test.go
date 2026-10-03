package smb2

import (
	"bytes"
	"sync"
	"testing"
	"time"

	. "github.com/djosh34/s3-smb/internal/smb-old/smb2/internal/erref"
	. "github.com/djosh34/s3-smb/internal/smb-old/smb2/internal/smb2"
	"github.com/djosh34/s3-smb/internal/smb-old/smb2/vfs"
)

type pausedXattrFS struct {
	*rangeXattrFS
	once             sync.Once
	entered, release chan struct{}
}

func (f *pausedXattrFS) Getxattr(h vfs.VfsHandle, k string, b []byte) (int, error) {
	n, e := f.rangeXattrFS.Getxattr(h, k, b)
	if b != nil {
		f.once.Do(func() { close(f.entered); <-f.release })
	}
	return n, e
}
func (f *rangeXattrFS) Removexattr(vfs.VfsHandle, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getErr = missingXattrError
	f.value = nil
	return nil
}

func TestResourceForkNativeMaximumAndMissing(t *testing.T) {
	for _, size := range []int{vfs.MaxXattrSize - 1, vfs.MaxXattrSize} {
		f := newRangeXattrFS(bytes.Repeat([]byte{'a'}, size))
		tree, id, out := wireTree(t, f)
		open := tree.conn.serverCtx.getOpen(7)
		open.isEa = true
		req := &WriteRequest{FileId: id, Offset: vfs.MaxXattrSize - 1, Data: []byte{'z'}}
		if e := tree.writeImpl(nil, requestBytes(req), id, open, 0); e != nil {
			t.Fatal(e)
		}
		if got := wireStatus(t, out); got != STATUS_SUCCESS {
			t.Fatal(got)
		}
		got := f.snapshot()
		if len(got) != vfs.MaxXattrSize || got[len(got)-1] != 'z' || !bytes.Equal(got[:len(got)-1], bytes.Repeat([]byte{'a'}, vfs.MaxXattrSize-1)) {
			t.Fatal("exact native maximum corrupt")
		}
	}
	f := newRangeXattrFS(nil)
	f.getErr = missingXattrError
	tree, id, out := wireTree(t, f)
	open := tree.conn.serverCtx.getOpen(7)
	open.isEa = true
	if e := tree.writeImpl(nil, requestBytes(&WriteRequest{FileId: id, Offset: 2, Data: []byte{'x'}}), id, open, 0); e != nil {
		t.Fatal(e)
	}
	if got := wireStatus(t, out); got != STATUS_SUCCESS {
		t.Fatal(got)
	}
	if !bytes.Equal(f.snapshot(), []byte{0, 0, 'x'}) {
		t.Fatal("missing stream not initialized with zero hole")
	}
}

func TestResourceForkMutationsSerializeAcrossSessions(t *testing.T) {
	for _, operation := range []string{"range", "resize", "whole-set", "create-truncate", "remove"} {
		t.Run(operation, func(t *testing.T) {
			f := &pausedXattrFS{rangeXattrFS: newRangeXattrFS([]byte("abcdef")), entered: make(chan struct{}), release: make(chan struct{})}
			t.Cleanup(func() {
				select {
				case <-f.release:
				default:
					close(f.release)
				}
			})
			first, id, out := wireTree(t, f)
			open := first.conn.serverCtx.getOpen(7)
			open.isEa = true
			open.eaKey = "AFP_Resource"
			second, secondID, secondOut := wireTree(t, f)
			second.conn.serverCtx = first.conn.serverCtx
			secondID.SetHandleId(8)
			secondOpen := &Open{fileId: 8, durableFileId: 42, session: second.session, tree: &second.treeConn, isEa: true, eaKey: "AFP_Resource"}
			first.conn.serverCtx.addOpen(secondOpen)
			firstDone := make(chan error, 1)
			go func() {
				firstDone <- first.writeImpl(nil, requestBytes(&WriteRequest{FileId: id, Offset: 1, Data: []byte{'X'}}), id, open, 0)
			}()
			select {
			case <-f.entered:
			case <-time.After(time.Second):
				t.Fatal("first RMW did not reach read")
			}
			secondDone := make(chan error, 1)
			started := make(chan struct{})
			go func() {
				close(started)
				var e error
				switch operation {
				case "range":
					e = second.writeImpl(nil, requestBytes(&WriteRequest{FileId: secondID, Offset: 4, Data: []byte{'Y'}}), secondID, secondOpen, 0)
				case "resize":
					e = second.resizeXattr(8, "AFP_Resource", 4)
				case "whole-set":
					e = second.setXattr(8, "AFP_Resource", []byte("whole"))
				case "create-truncate":
					_, e = second.handleCreateEA(FILE_OVERWRITE_IF, 8, "AFP_Resource")
				case "remove":
					e = second.removeXattr(8, "AFP_Resource")
				}
				secondDone <- e
			}()
			<-started
			// The competing operation cannot finish while the first old-value snapshot
			// is paused. This deterministically catches a missing shared lock.
			var premature error
			early := false
			select {
			case premature = <-secondDone:
				early = true
			case <-time.After(20 * time.Millisecond):
			}
			close(f.release)
			select {
			case e := <-firstDone:
				if e != nil {
					t.Fatal(e)
				}
			case <-time.After(time.Second):
				t.Fatal("first writer stuck")
			}
			if early {
				t.Fatalf("competing %s escaped RMW lock: %v", operation, premature)
			}
			select {
			case e := <-secondDone:
				if e != nil {
					t.Fatal(e)
				}
			case <-time.After(time.Second):
				t.Fatal("second mutation stuck")
			}
			if got := wireStatus(t, out); got != STATUS_SUCCESS {
				t.Fatal(got)
			}
			want := []byte("aXcdYf")
			switch operation {
			case "range":
				if got := wireStatus(t, secondOut); got != STATUS_SUCCESS {
					t.Fatal(got)
				}
			case "resize":
				want = []byte("aXcd")
			case "whole-set":
				want = []byte("whole")
			case "create-truncate", "remove":
				want = nil
			}
			if !bytes.Equal(f.snapshot(), want) {
				t.Fatalf("value = %q, want %q", f.snapshot(), want)
			}
		})
	}
}
