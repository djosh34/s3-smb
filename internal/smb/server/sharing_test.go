package server

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/smbtest"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

// openWith tries to open or create name and returns the CREATE status.
func openWith(t *testing.T, client *testClient, name string, access, share, disposition uint32) (wire.FileID, smb.Status) {
	t.Helper()
	result, status := client.create(t, smbtest.CreateOptions{Request: wire.CreateRequest{Name: name, DesiredAccess: access, ShareAccess: share, Disposition: disposition}})
	return result.Reply.ID, status
}

// Each open's access must pass every other open's share mode, both ways.
// Reading attributes alone ignores sharing, and each stream has its own,
// except that deleting a file needs every stream to share delete.
func TestSharingModes(t *testing.T) {
	const shareRead, shareWrite, shareDelete = 1, 2, 4
	srv := newTestServer(t)
	first, second := srv.connect(t), srv.connect(t)
	for i, test := range []struct {
		name                      string
		firstName, secondName     string
		firstAccess, firstShare   uint32
		secondAccess, secondShare uint32
		want                      smb.Status
	}{
		{"readers sharing read", "f", "f", fileReadData, shareRead, fileReadData, shareRead, smb.StatusSuccess},
		{"writer after a reader denying write", "f", "f", fileReadData, shareRead, fileWriteData, 7, smb.StatusSharingViolation},
		{"reader denying write after a writer", "f", "f", fileWriteData, 7, fileReadData, shareRead, smb.StatusSharingViolation},
		{"readers and writers sharing both", "f", "f", fileReadData | fileWriteData, shareRead | shareWrite, fileReadData | fileWriteData, shareRead | shareWrite, smb.StatusSuccess},
		{"delete after a reader denying delete", "f", "f", fileReadData, shareRead | shareWrite, fileDelete, 7, smb.StatusSharingViolation},
		{"reader denying delete after a delete", "f", "f", fileDelete, 7, fileReadData, shareRead | shareWrite, smb.StatusSharingViolation},
		{"append counts as write", "f", "f", fileReadData, shareRead | shareDelete, fileAppendData, 7, smb.StatusSharingViolation},
		{"execute counts as read", "f", "f", fileWriteData, shareWrite | shareDelete, fileExecute, 7, smb.StatusSharingViolation},
		{"attributes beside an exclusive writer", "f", "f", fileWriteData, 0, 0x80, 0, smb.StatusSuccess},
		{"exclusive writer beside attributes", "f", "f", 0x80, 0, fileWriteData, 0, smb.StatusSuccess},
		{"exclusive opens of two streams", "f:a", "f:b", fileReadData | fileWriteData, 0, fileReadData | fileWriteData, 0, smb.StatusSuccess},
		{"file delete after a stream denying delete", "f:a", "f", fileReadData, shareRead | shareWrite, fileDelete, 7, smb.StatusSharingViolation},
		{"stream denying delete after a file delete", "f", "f:a", fileDelete, 7, fileReadData, shareRead | shareWrite, smb.StatusSharingViolation},
	} {
		file := fmt.Sprint("file", i)
		closeOK(t, first, first.open(t, file))
		id, status := openWith(t, first, strings.Replace(test.firstName, "f", file, 1), test.firstAccess, test.firstShare, fileOpenIf)
		if status != smb.StatusSuccess {
			t.Fatalf("%s: first open status %#x", test.name, status)
		}
		other, status := openWith(t, second, strings.Replace(test.secondName, "f", file, 1), test.secondAccess, test.secondShare, fileOpenIf)
		if status != test.want {
			t.Errorf("%s: status %#x, want %#x", test.name, status, test.want)
		}
		if status == smb.StatusSuccess {
			closeOK(t, second, other)
		}
		closeOK(t, first, id)
	}
}

// A destructive CREATE refused for sharing leaves the data alone.
func TestSharingRefusalKeepsData(t *testing.T) {
	srv := newTestServer(t)
	first, second := srv.connect(t), srv.connect(t)
	seed(t, first, "file", "existing bytes")
	id := openAs(t, first, "file", fileReadData, 1, 0)
	for _, disposition := range []uint32{fileSupersede, fileOverwrite, fileOverwriteIf} {
		if _, status := openWith(t, second, "file", fileWriteData|fileDelete, 7, disposition); status != smb.StatusSharingViolation {
			t.Fatalf("disposition %d: status %#x", disposition, status)
		}
	}
	srv.expectContent(t, map[string]string{"file": "existing bytes"})
	closeOK(t, first, id)
	overwritten, status := openWith(t, second, "file", fileWriteData, 7, fileOverwrite)
	if status != smb.StatusSuccess {
		t.Fatalf("overwrite after the reader closed: status %#x", status)
	}
	closeOK(t, second, overwritten)
	if got := srv.content(t, "file"); got != "" {
		t.Fatalf("overwritten file holds %q", got)
	}
}

// Sharing ends with the last conflicting open, however it ends.
func TestSharingEndsWithTheOpen(t *testing.T) {
	srv := newTestServer(t)
	first, second := srv.connect(t), srv.connect(t)
	ids := []wire.FileID{openAs(t, first, "closed", fileReadData, 1, 0), openAs(t, first, "closed", fileReadData, 1, 0)}
	openAs(t, first, "dropped", fileReadData, 1, 0)
	for _, id := range ids {
		if _, status := openWith(t, second, "closed", fileWriteData, 7, fileOpen); status != smb.StatusSharingViolation {
			t.Fatalf("writer beside a live reader: status %#x", status)
		}
		closeOK(t, first, id)
	}
	first.drop(t)
	for _, name := range []string{"closed", "dropped"} {
		closeOK(t, second, openAs(t, second, name, fileWriteData, 0, 0))
	}
}

// A CREATE that fails after taking its share mode gives it back and closes
// its storage handle.
func TestSharingFailedCreateReleasesItsShareMode(t *testing.T) {
	srv := newTestServer(t)
	client := srv.connect(t)
	closeOK(t, client, client.open(t, "file"))
	var opened, closed atomic.Int32
	srv.faults.set(func(hooks *storageHooks) {
		hooks.Open = func(ctx context.Context, object smb.ObjectKey, access smb.Access) (smb.Handle, error) {
			opened.Add(1)
			return srv.adapter.Open(ctx, object, access)
		}
		hooks.Close = func(ctx context.Context, handle smb.Handle) error {
			closed.Add(1)
			return srv.adapter.Close(ctx, handle)
		}
		hooks.GetAttr = func(context.Context, smb.ObjectKey) (smb.Attr, error) { return smb.Attr{}, smb.ErrIO }
	})
	if _, status := openWith(t, client, "file", fileWriteData, 1, fileOpen); status != smb.StatusIODeviceError {
		t.Fatalf("CREATE status %#x", status)
	}
	srv.faults.set(func(hooks *storageHooks) { *hooks = storageHooks{} })
	if opened.Load() != 1 || closed.Load() != 1 {
		t.Fatalf("%d storage handles opened, %d closed; want 1", opened.Load(), closed.Load())
	}
	closeOK(t, client, openAs(t, client, "file", fileWriteData, 7, 0))
}
