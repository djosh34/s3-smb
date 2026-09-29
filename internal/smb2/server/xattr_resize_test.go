package smb2

import (
	"bytes"
	"syscall"
	"testing"

	. "github.com/djosh34/s3-smb/internal/smb2/internal/erref"
	. "github.com/djosh34/s3-smb/internal/smb2/internal/smb2"
	"github.com/djosh34/s3-smb/internal/smb2/vfs"
)

func TestResourceForkResizePreservesData(t *testing.T) {
	for _, size := range []int64{3, 9} {
		f := newRangeXattrFS([]byte("abcdef"))
		tree, id, out := wireTree(t, f)
		req := &SetInfoRequest{FileId: id, InfoType: SMB2_0_INFO_FILE, FileInfoClass: FileEndOfFileInformation, Input: &FileEndOfFileInformationEncoder{EndOfFile: size}}
		if err := tree.setEndOfFileInfoEa(nil, id, "AFP_Resource", requestBytes(req)); err != nil {
			t.Fatal(err)
		}
		if got := wireStatus(t, out); got != STATUS_SUCCESS {
			t.Fatal(got)
		}
		want := make([]byte, size)
		copy(want, []byte("abcdef"))
		if !bytes.Equal(f.snapshot(), want) {
			t.Fatalf("resize to %d: got %q want %q", size, f.snapshot(), want)
		}
	}
}

type truncateErrorFS struct{ *faultFS }

func (*truncateErrorFS) Truncate(vfs.VfsHandle, uint64) error { return syscall.EIO }
func TestEndOfFileFailureIsNotSuccess(t *testing.T) {
	f := &truncateErrorFS{&faultFS{deleteDispositionFS: newDeleteDispositionFS(vfs.FileTypeRegularFile)}}
	tree, id, out := wireTree(t, f)
	req := &SetInfoRequest{FileId: id, InfoType: SMB2_0_INFO_FILE, FileInfoClass: FileEndOfFileInformation, Input: &FileEndOfFileInformationEncoder{EndOfFile: 3}}
	if err := tree.setEndOfFileInfo(nil, id, requestBytes(req)); err != nil {
		t.Fatal(err)
	}
	if got := wireStatus(t, out); got != STATUS_IO_DEVICE_ERROR {
		t.Fatalf("truncate failure reported %v", got)
	}
}

func TestResourceForkResizeBoundsBeforeMutation(t *testing.T) {
	for _, size := range []int64{-1, vfs.MaxXattrSize + 1, 1 << 50} {
		f := newRangeXattrFS([]byte("keep"))
		tree, id, out := wireTree(t, f)
		req := &SetInfoRequest{FileId: id, InfoType: SMB2_0_INFO_FILE, FileInfoClass: FileEndOfFileInformation, Input: &FileEndOfFileInformationEncoder{EndOfFile: size}}
		if err := tree.setEndOfFileInfoEa(nil, id, "AFP_Resource", requestBytes(req)); err != nil {
			t.Fatal(err)
		}
		want := STATUS_EA_TOO_LARGE
		if size < 0 {
			want = STATUS_INVALID_PARAMETER
		}
		if got := wireStatus(t, out); got != want {
			t.Fatalf("size %d: %v, want %v", size, got, want)
		}
		if f.probes != 0 || f.sets != 0 || !bytes.Equal(f.snapshot(), []byte("keep")) {
			t.Fatal("invalid resize touched native value")
		}
	}
}

func TestResourceForkOpenErrorDoesNotOverwrite(t *testing.T) {
	for _, disp := range []uint32{FILE_OPEN_IF, FILE_CREATE, FILE_OVERWRITE} {
		f := newRangeXattrFS([]byte("keep"))
		f.getErr = syscall.EIO
		tree, _, _ := wireTree(t, f)
		_, err := tree.handleCreateEA(disp, 7, "AFP_Resource")
		if err == nil || f.sets != 0 || !bytes.Equal(f.snapshot(), []byte("keep")) {
			t.Fatalf("disposition %d: error=%v sets=%d value=%q", disp, err, f.sets, f.snapshot())
		}
	}
}
