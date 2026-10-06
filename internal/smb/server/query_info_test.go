package server

import (
	"bytes"
	"context"
	"encoding/binary"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func expectInfo[T comparable](t *testing.T, client *testClient, id wire.FileID, class wire.FileInfoClass, decoder func([]byte) (T, error), want T) {
	t.Helper()
	if got := queryClass(t, client, id, class, decoder); got != want {
		t.Errorf("class %d = %+v, want %+v", class, got, want)
	}
}

func TestQueryInfoFileClasses(t *testing.T) {
	srv := newTestServer(t)
	client := srv.connect(t)
	directory := openDirectory(t, client, "directory")
	file := client.open(t, "directory/file")
	// The file's data is still buffered; queries must report it.
	if status := client.write(t, wire.WriteRequest{ID: file, Offset: 4096, Data: []byte("buffered length")}); status != smb.StatusSuccess {
		t.Fatalf("WRITE status %#x", status)
	}
	inode := uint64(srv.object(t, "directory/file"))
	basic := basicInfo(t, client, file)
	standard := wire.FileStandardInformation{AllocationSize: 8192, EndOfFile: 4111, Links: 1}
	name := wire.FileNameInformation{Name: "\\directory\\file"}
	expectInfo(t, client, file, wire.ClassFileStandard, wire.DecodeFileStandardInformation, standard)
	expectInfo(t, client, file, wire.ClassFileInternal, wire.DecodeFileInternalInformation, wire.FileInternalInformation{Index: inode})
	expectInfo(t, client, file, wire.ClassFileAccess, wire.DecodeFileAccessInformation, wire.FileAccessInformation{Access: fileAllAccess})
	expectInfo(t, client, file, wire.ClassFileName, wire.DecodeFileNameInformation, name)
	expectInfo(t, client, file, wire.ClassFileEA, wire.DecodeFileEAInformation, wire.FileEAInformation{})
	expectInfo(t, client, file, wire.ClassFilePosition, wire.DecodeFilePositionInformation, wire.FilePositionInformation{})
	expectInfo(t, client, file, wire.ClassFileMode, wire.DecodeFileModeInformation, wire.FileModeInformation{})
	expectInfo(t, client, file, wire.ClassFileAlignment, wire.DecodeFileAlignmentInformation, wire.FileAlignmentInformation{})
	expectInfo(t, client, file, wire.ClassFileAttributeTag, wire.DecodeFileAttributeTagInformation, wire.FileAttributeTagInformation{Attributes: basic.Attributes})
	expectInfo(t, client, file, wire.ClassFileNetworkOpen, wire.DecodeFileNetworkOpenInformation, wire.FileNetworkOpenInformation{
		Created: basic.Created, Accessed: basic.Accessed, Modified: basic.Modified, Changed: basic.Changed,
		AllocationSize: standard.AllocationSize, EndOfFile: standard.EndOfFile, Attributes: basic.Attributes,
	})
	expectInfo(t, client, file, wire.ClassFileAll, wire.DecodeFileAllInformation, wire.FileAllInformation{
		Name: name, Basic: basic, Standard: standard, Internal: wire.FileInternalInformation{Index: inode}, Access: wire.FileAccessInformation{Access: fileAllAccess},
	})
	space, err := srv.storage.StatFS(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	fileID := wire.FileIDInformation{VolumeSerial: space.VolumeID}
	binary.LittleEndian.PutUint64(fileID.ID[:8], inode)
	expectInfo(t, client, file, wire.ClassFileID, wire.DecodeFileIDInformation, fileID)
	expectInfo(t, client, client.open(t, "directory/file"), wire.ClassFileID, wire.DecodeFileIDInformation, fileID)

	if got := queryClass(t, client, directory, wire.ClassFileStandard, wire.DecodeFileStandardInformation); !got.Directory {
		t.Errorf("directory standard information %+v", got)
	}
}

func TestQueryInfoFollowsRenameAndDelete(t *testing.T) {
	client := newTestServer(t).connect(t)
	id := client.open(t, "before")
	if status := rename(t, client, id, "after", false); status != smb.StatusSuccess {
		t.Fatalf("rename status %#x", status)
	}
	if status := setDeletePending(t, client, id, true); status != smb.StatusSuccess {
		t.Fatalf("SET_INFO status %#x", status)
	}
	all := queryClass(t, client, id, wire.ClassFileAll, wire.DecodeFileAllInformation)
	if all.Name.Name != "\\after" || !all.Standard.DeletePending {
		t.Fatalf("all information %+v", all)
	}
	if !queryClass(t, client, id, wire.ClassFileStandard, wire.DecodeFileStandardInformation).DeletePending {
		t.Fatal("standard information without the pending delete")
	}
}

// Fixed-size classes need their whole size; the variable part of a name is
// cut short with STATUS_BUFFER_OVERFLOW.
func TestQueryInfoShortBuffers(t *testing.T) {
	client := newTestServer(t).connect(t)
	id := client.open(t, "a-long-file-name")
	query := func(class wire.FileInfoClass, length uint32) ([]byte, smb.Status) {
		return client.queryInfo(t, wire.QueryInfoRequest{ID: id, InfoType: wire.InfoFile, InfoClass: uint8(class), OutputLength: length})
	}
	for _, test := range []struct {
		class  wire.FileInfoClass
		length uint32
		want   smb.Status
	}{
		{wire.ClassFileBasic, 39, smb.StatusInfoLengthMismatch},
		{wire.ClassFileBasic, 40, smb.StatusSuccess},
		{wire.ClassFileStandard, 23, smb.StatusInfoLengthMismatch},
		{wire.ClassFileName, 3, smb.StatusInfoLengthMismatch},
		{wire.ClassFileName, 4, smb.StatusBufferOverflow},
		{wire.ClassFileName, 37, smb.StatusBufferOverflow},
		{wire.ClassFileName, 38, smb.StatusSuccess},
		{wire.ClassFileAll, 99, smb.StatusInfoLengthMismatch},
		{wire.ClassFileAll, 100, smb.StatusBufferOverflow},
	} {
		full, status := query(test.class, 4096)
		if status != smb.StatusSuccess {
			t.Fatalf("class %d: status %#x", test.class, status)
		}
		data, status := query(test.class, test.length)
		if status != test.want || status != smb.StatusInfoLengthMismatch && !bytes.Equal(data, full[:test.length]) {
			t.Errorf("class %d in %d bytes = %x, %#x; want %#x", test.class, test.length, data, status, test.want)
		}
	}
}

func TestQueryInfoRefusals(t *testing.T) {
	client := newTestServer(t).connect(t)
	id := client.open(t, "file")
	closed := id
	closed.Volatile++
	for _, test := range []struct {
		name     string
		id       wire.FileID
		infoType wire.InfoType
		class    uint8
		length   uint32
		want     smb.Status
	}{
		{"normalized name", id, wire.InfoFile, 48, 4096, smb.StatusNotSupported},
		{"streams", id, wire.InfoFile, 22, 4096, smb.StatusInvalidInfoClass},
		{"set-only class", id, wire.InfoFile, uint8(wire.ClassFileRename), 4096, smb.StatusInvalidInfoClass},
		{"unknown file class", id, wire.InfoFile, 200, 4096, smb.StatusInvalidInfoClass},
		{"object ID", id, wire.InfoFilesystem, 8, 4096, smb.StatusNotSupported},
		{"unknown filesystem class", id, wire.InfoFilesystem, 100, 4096, smb.StatusInvalidInfoClass},
		{"security", id, wire.InfoSecurity, 0, 4096, smb.StatusNotSupported},
		{"quota", id, 4, 0, 4096, smb.StatusNotSupported},
		{"unknown type", id, 5, 0, 4096, smb.StatusInvalidParameter},
		{"buffer too large", id, wire.InfoFile, uint8(wire.ClassFileBasic), smb.MaxTransactSize + 1, smb.StatusInvalidParameter},
		{"closed file", closed, wire.InfoFile, uint8(wire.ClassFileBasic), 4096, smb.StatusFileClosed},
	} {
		if _, status := client.queryInfo(t, wire.QueryInfoRequest{ID: test.id, InfoType: test.infoType, InfoClass: test.class, OutputLength: test.length}); status != test.want {
			t.Errorf("%s: status %#x, want %#x", test.name, status, test.want)
		}
	}
	client.echo(t)
}

func queryFilesystem(t *testing.T, client *testClient, id wire.FileID, class wire.FilesystemInfoClass, length uint32) ([]byte, smb.Status) {
	t.Helper()
	return client.queryInfo(t, wire.QueryInfoRequest{ID: id, InfoType: wire.InfoFilesystem, InfoClass: uint8(class), OutputLength: length})
}

// Finder and Time Machine size a backup from these replies: space in 4 KiB
// units of 8 sectors of 512 bytes, rounded down.
func TestFilesystemInfo(t *testing.T) {
	srv := newTestServer(t)
	client := srv.connect(t)
	id := client.open(t, "file")
	actual, err := srv.storage.StatFS(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if data, status := queryFilesystem(t, client, id, wire.ClassFilesystemVolume, 1024); status != smb.StatusSuccess {
		t.Fatalf("volume status %#x", status)
	} else if volume, err := wire.DecodeFilesystemVolumeInformation(data); err != nil || volume != (wire.FilesystemVolumeInformation{Label: "backup", Serial: uint32(actual.VolumeID & 0xffffffff)}) {
		t.Fatalf("volume = %+v, %v", volume, err)
	}
	if data, status := queryFilesystem(t, client, id, wire.ClassFilesystemDevice, 1024); status != smb.StatusSuccess {
		t.Fatalf("device status %#x", status)
	} else if device, err := wire.DecodeFilesystemDeviceInformation(data); err != nil || device != (wire.FilesystemDeviceInformation{Type: 7, Characteristics: 0x10}) {
		t.Fatalf("device = %+v, %v", device, err)
	}
	if data, status := queryFilesystem(t, client, id, wire.ClassFilesystemAttribute, 1024); status != smb.StatusSuccess {
		t.Fatalf("attribute status %#x", status)
	} else if attribute, err := wire.DecodeFilesystemAttributeInformation(data); err != nil || attribute != (wire.FilesystemAttributeInformation{Name: "s3-smb", Attributes: smb.AdvertisedFilesystemAttributes, MaxComponentLength: 255}) {
		t.Fatalf("attribute = %+v, %v", attribute, err)
	}

	expectSpace := func(space smb.Space) {
		t.Helper()
		units := func(size uint64) []byte { return binary.LittleEndian.AppendUint64(nil, size/4096) }
		geometry := []byte{8, 0, 0, 0, 0, 2, 0, 0}
		for _, test := range []struct {
			want  [][]byte
			class wire.FilesystemInfoClass
		}{
			{[][]byte{units(space.Capacity), units(space.Available), geometry}, wire.ClassFilesystemSize},
			{[][]byte{units(space.Capacity), units(space.Available), units(space.Free), geometry}, wire.ClassFilesystemFullSize},
		} {
			data, status := queryFilesystem(t, client, id, test.class, 1024)
			if want := bytes.Join(test.want, nil); status != smb.StatusSuccess || !bytes.Equal(data, want) {
				t.Fatalf("class %d = %x, %#x; want %x", test.class, data, status, want)
			}
		}
	}
	expectSpace(actual)
	configured := smb.Space{Capacity: 3<<30 + 1023, Free: 2<<30 + 4095, Available: 1<<30 + 1}
	srv.faults.set(func(hooks *storageHooks) {
		hooks.StatFS = func(context.Context) (smb.Space, error) { return configured, nil }
	})
	expectSpace(configured)

	srv.faults.set(func(hooks *storageHooks) {
		hooks.StatFS = func(context.Context) (smb.Space, error) { return smb.Space{}, smb.ErrIO }
	})
	for _, class := range []wire.FilesystemInfoClass{wire.ClassFilesystemVolume, wire.ClassFilesystemSize, wire.ClassFilesystemFullSize} {
		if _, status := queryFilesystem(t, client, id, class, 1024); status != smb.StatusIODeviceError {
			t.Fatalf("class %d without StatFS: status %#x", class, status)
		}
	}
	// These classes need no space figures.
	for _, class := range []wire.FilesystemInfoClass{wire.ClassFilesystemDevice, wire.ClassFilesystemAttribute} {
		if _, status := queryFilesystem(t, client, id, class, 1024); status != smb.StatusSuccess {
			t.Fatalf("class %d without StatFS: status %#x", class, status)
		}
	}
}

func TestFilesystemInfoOutputLengths(t *testing.T) {
	client := newTestServer(t).connect(t)
	id := client.open(t, "file")
	for _, test := range []struct {
		class         wire.FilesystemInfoClass
		minimum, full uint32
	}{
		{wire.ClassFilesystemVolume, 18, 30},
		{wire.ClassFilesystemSize, 24, 24},
		{wire.ClassFilesystemFullSize, 32, 32},
		{wire.ClassFilesystemDevice, 8, 8},
		{wire.ClassFilesystemAttribute, 12, 24},
	} {
		full, status := queryFilesystem(t, client, id, test.class, 1024)
		if status != smb.StatusSuccess || len(full) != int(test.full) {
			t.Fatalf("class %d = %d bytes, %#x", test.class, len(full), status)
		}
		if _, status := queryFilesystem(t, client, id, test.class, test.minimum-1); status != smb.StatusInfoLengthMismatch {
			t.Errorf("class %d below its minimum: status %#x", test.class, status)
		}
		for length := test.minimum; length <= test.full; length++ {
			want := smb.StatusBufferOverflow
			if length == test.full {
				want = smb.StatusSuccess
			}
			if data, status := queryFilesystem(t, client, id, test.class, length); status != want || !bytes.Equal(data, full[:length]) {
				t.Errorf("class %d in %d bytes = %x, %#x", test.class, length, data, status)
			}
		}
	}
}
