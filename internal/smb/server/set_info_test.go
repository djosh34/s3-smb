package server

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/smbtest"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

// queryClass returns a file information class of id, failing the test unless
// the query succeeds.
func queryClass[T any](t *testing.T, client *testClient, id wire.FileID, class wire.FileInfoClass, decoder func([]byte) (T, error)) T {
	t.Helper()
	data, status := client.queryInfo(t, wire.QueryInfoRequest{ID: id, InfoType: wire.InfoFile, InfoClass: uint8(class), OutputLength: 4096})
	if status != smb.StatusSuccess {
		t.Fatalf("QUERY_INFO class %d: status %#x", class, status)
	}
	value, err := decoder(data)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func basicInfo(t *testing.T, client *testClient, id wire.FileID) wire.FileBasicInformation {
	t.Helper()
	return queryClass(t, client, id, wire.ClassFileBasic, wire.DecodeFileBasicInformation)
}

func endOfFile(t *testing.T, client *testClient, id wire.FileID) uint64 {
	t.Helper()
	return queryClass(t, client, id, wire.ClassFileStandard, wire.DecodeFileStandardInformation).EndOfFile
}

func setBasic(t *testing.T, client *testClient, id wire.FileID, info wire.FileBasicInformation) smb.Status {
	t.Helper()
	return setFileInfo(t, client, id, wire.ClassFileBasic, wire.EncodeFileBasicInformation, info)
}

func setAllocation(t *testing.T, client *testClient, id wire.FileID, size uint64) smb.Status {
	t.Helper()
	return setFileInfo(t, client, id, wire.ClassFileAllocation, wire.EncodeFileAllocationInformation, wire.FileAllocationInformation{AllocationSize: size})
}

func setEndOfFile(t *testing.T, client *testClient, id wire.FileID, size uint64) smb.Status {
	t.Helper()
	return setFileInfo(t, client, id, wire.ClassFileEndOfFile, wire.EncodeFileEndOfFileInformation, wire.FileEndOfFileInformation{EndOfFile: size})
}

func toFiletime(t *testing.T, value time.Time) wire.Filetime {
	t.Helper()
	converted, err := wire.EncodeFiletime(value)
	if err != nil {
		t.Fatal(err)
	}
	return converted
}

// writeFile writes data at the start of id.
func writeFile(t *testing.T, client *testClient, id wire.FileID, data []byte) {
	t.Helper()
	if status := client.write(t, wire.WriteRequest{ID: id, Data: data}); status != smb.StatusSuccess {
		t.Fatalf("WRITE status %#x", status)
	}
}

// Regression for #111: the FILETIME sentinels leave stored times alone and
// do not stop later writes from updating them.
func TestSetInfoTimeSentinels(t *testing.T) {
	client := newTestServer(t).connect(t)
	id := client.open(t, "file")
	explicit := toFiletime(t, time.Date(2001, 1, 2, 3, 4, 5, 0, time.UTC))
	if status := setBasic(t, client, id, wire.FileBasicInformation{Created: explicit, Accessed: explicit, Modified: explicit, Changed: explicit, Attributes: 0x20}); status != smb.StatusSuccess {
		t.Fatalf("SET_INFO status %#x", status)
	}
	before := basicInfo(t, client, id)
	for _, sentinel := range []wire.Filetime{wire.FiletimeUnchanged, wire.FiletimeSuppress, wire.FiletimeResume} {
		if status := setBasic(t, client, id, wire.FileBasicInformation{Created: sentinel, Accessed: sentinel, Modified: sentinel, Changed: sentinel}); status != smb.StatusSuccess {
			t.Fatalf("sentinel %#x: status %#x", sentinel, status)
		}
		if after := basicInfo(t, client, id); after != before {
			t.Fatalf("sentinel %#x changed %+v to %+v", sentinel, before, after)
		}
	}
	writeFile(t, client, id, []byte("later"))
	if after := basicInfo(t, client, id); after.Modified <= before.Modified || after.Changed <= before.Changed {
		t.Fatalf("WRITE after sentinels left times at %+v", after)
	}
}

// Regression for #111: a negative time in any field fails the whole request.
func TestSetInfoNegativeTimeChangesNothing(t *testing.T) {
	client := newTestServer(t).connect(t)
	id := client.open(t, "file")
	valid := toFiletime(t, time.Date(2001, 1, 2, 3, 4, 5, 0, time.UTC))
	before := basicInfo(t, client, id)
	for _, invalid := range []wire.Filetime{1 << 63, wire.FiletimeResume - 1} {
		for field := range 4 {
			times := [4]wire.Filetime{valid, valid, valid, valid}
			times[field] = invalid
			info := wire.FileBasicInformation{Created: times[0], Accessed: times[1], Modified: times[2], Changed: times[3], Attributes: 0x20}
			if status := setBasic(t, client, id, info); status != smb.StatusInvalidParameter {
				t.Fatalf("time %#x in field %d: status %#x", invalid, field, status)
			}
		}
	}
	if after := basicInfo(t, client, id); after != before {
		t.Fatalf("refused requests changed %+v to %+v", before, after)
	}
}

func TestSetInfoBasicFields(t *testing.T) {
	client := newTestServer(t).connect(t)
	id := client.open(t, "file")
	// The Unix epoch is an ordinary time, not a sentinel.
	epoch := toFiletime(t, time.Unix(0, 0))
	if status := setBasic(t, client, id, wire.FileBasicInformation{Created: epoch, Accessed: epoch, Modified: epoch, Changed: epoch}); status != smb.StatusSuccess {
		t.Fatalf("SET_INFO status %#x", status)
	}
	created := toFiletime(t, time.Date(1985, 3, 4, 5, 6, 7, 800, time.UTC))
	if status := setBasic(t, client, id, wire.FileBasicInformation{Created: created}); status != smb.StatusSuccess {
		t.Fatalf("SET_INFO status %#x", status)
	}
	if got := basicInfo(t, client, id); got.Created != created || got.Accessed != epoch || got.Modified != epoch || got.Changed != epoch {
		t.Fatalf("times = %+v", got)
	}

	// Clients set only the seven settable attributes; NORMAL stands alone
	// and zero leaves the attributes as they are.
	for _, step := range []struct{ set, want uint32 }{
		{0x3127, 0x3127}, {0, 0x3127}, {0x620, 0x20}, {0x600, 0x80}, {0x80, 0x80}, {0x2, 0x2},
	} {
		if status := setBasic(t, client, id, wire.FileBasicInformation{Attributes: step.set}); status != smb.StatusSuccess {
			t.Fatalf("attributes %#x: status %#x", step.set, status)
		}
		if got := basicInfo(t, client, id).Attributes; got != step.want {
			t.Fatalf("attributes %#x stored as %#x, want %#x", step.set, got, step.want)
		}
	}
	closeOK(t, client, id)
	if reopened := mustCreate(t, client, smbtest.CreateOptions{Request: wire.CreateRequest{Name: "file", DesiredAccess: fileReadData, ShareAccess: 7, Disposition: fileOpen}}); reopened.Reply.Attributes != 0x2 {
		t.Fatalf("reopened attributes %#x", reopened.Reply.Attributes)
	}

	directory := openAs(t, client, "directory", fileAllAccess, 7, fileDirectoryFile)
	if status := setBasic(t, client, directory, wire.FileBasicInformation{Attributes: 0x3027}); status != smb.StatusSuccess {
		t.Fatalf("directory attributes: status %#x", status)
	}
	if got := basicInfo(t, client, directory).Attributes; got != 0x3037 {
		t.Fatalf("directory attributes %#x", got)
	}
	// DIRECTORY on a file and TEMPORARY on a directory are refused.
	file := client.open(t, "file")
	for _, test := range []struct {
		id         wire.FileID
		attributes uint32
	}{{file, 0x10}, {directory, 0x100}} {
		before := basicInfo(t, client, test.id)
		if status := setBasic(t, client, test.id, wire.FileBasicInformation{Created: created, Attributes: test.attributes}); status != smb.StatusInvalidParameter {
			t.Fatalf("attributes %#x: status %#x", test.attributes, status)
		}
		if after := basicInfo(t, client, test.id); after != before {
			t.Fatalf("refused request changed %+v to %+v", before, after)
		}
	}
}

// Regression for #108: allocation rounds up to whole 4 KiB clusters and
// shrinks a longer file to that size. A larger allocation is only a hint
// and changes nothing.
func TestSetInfoAllocation(t *testing.T) {
	client := newTestServer(t).connect(t)
	data := bytes.Repeat([]byte("123456789"), 1000)
	for _, test := range []struct{ allocation, want uint64 }{
		{0, 0}, {1, 4096}, {4095, 4096}, {4096, 4096}, {4097, 8192}, {8193, 9000}, {1<<63 - 4096, 9000},
	} {
		t.Run(fmt.Sprint(test.allocation), func(t *testing.T) {
			id := client.open(t, fmt.Sprint(test.allocation))
			writeFile(t, client, id, data)
			before := basicInfo(t, client, id)
			if status := setAllocation(t, client, id, test.allocation); status != smb.StatusSuccess {
				t.Fatalf("SET_INFO status %#x", status)
			}
			if test.want == uint64(len(data)) && basicInfo(t, client, id) != before {
				t.Fatal("an allocation hint changed the file times")
			}
			if status := client.flush(t, wire.FlushRequest{ID: id}); status != smb.StatusSuccess {
				t.Fatalf("FLUSH status %#x", status)
			}
			if got := endOfFile(t, client, id); got != test.want {
				t.Fatalf("EOF = %d, want %d", got, test.want)
			}
			read, status := client.read(t, wire.ReadRequest{ID: id, Length: 1 << 16})
			if test.want == 0 && status == smb.StatusEndOfFile {
				return
			}
			if status != smb.StatusSuccess || !bytes.Equal(read, data[:test.want]) {
				t.Fatalf("READ = %d bytes, %#x", len(read), status)
			}
		})
	}
}

func TestSetInfoEndOfFile(t *testing.T) {
	client := newTestServer(t).connect(t)
	id := client.open(t, "file")
	writeFile(t, client, id, []byte("1234567"))
	for _, size := range []uint64{3, 9} {
		if status := setEndOfFile(t, client, id, size); status != smb.StatusSuccess {
			t.Fatalf("EOF %d: status %#x", size, status)
		}
	}
	if status := client.flush(t, wire.FlushRequest{ID: id}); status != smb.StatusSuccess {
		t.Fatalf("FLUSH status %#x", status)
	}
	if data, status := client.read(t, wire.ReadRequest{ID: id, Length: 64}); status != smb.StatusSuccess || string(data) != "123\x00\x00\x00\x00\x00\x00" {
		t.Fatalf("READ = %q, %#x", data, status)
	}
}

func TestSetInfoChecksGrantedAccess(t *testing.T) {
	client := newTestServer(t).connect(t)
	writeFile(t, client, client.open(t, "file"), []byte("1234567"))
	attributes := wire.FileBasicInformation{Attributes: 0x20}
	for _, test := range []struct {
		name         string
		access       uint32
		basic, sizes smb.Status
	}{
		{"write attributes", 0x100, smb.StatusSuccess, smb.StatusAccessDenied},
		{"append data", fileAppendData, smb.StatusAccessDenied, smb.StatusAccessDenied},
		{"write data", fileWriteData, smb.StatusAccessDenied, smb.StatusSuccess},
	} {
		id := openAs(t, client, "file", test.access, 7, 0)
		if status := setBasic(t, client, id, attributes); status != test.basic {
			t.Errorf("%s: basic status %#x", test.name, status)
		}
		if status := setEndOfFile(t, client, id, 7); status != test.sizes {
			t.Errorf("%s: EOF status %#x", test.name, status)
		}
		if status := setAllocation(t, client, id, 8192); status != test.sizes {
			t.Errorf("%s: allocation status %#x", test.name, status)
		}
	}
}

func TestSetInfoRefusals(t *testing.T) {
	client := newTestServer(t).connect(t)
	file := client.open(t, "file")
	writeFile(t, client, file, []byte("1234567"))
	directory := openAs(t, client, "directory", fileAllAccess, 7, fileDirectoryFile)
	closed := file
	closed.Volatile++
	size := func(value uint64) []byte {
		return encode(t, wire.EncodeFileEndOfFileInformation, wire.FileEndOfFileInformation{EndOfFile: value})
	}
	for _, test := range []struct {
		name     string
		input    []byte
		id       wire.FileID
		want     smb.Status
		infoType wire.InfoType
		class    wire.FileInfoClass
	}{
		{"directory EOF", size(0), directory, smb.StatusInvalidParameter, wire.InfoFile, wire.ClassFileEndOfFile},
		{"directory allocation", size(0), directory, smb.StatusInvalidParameter, wire.InfoFile, wire.ClassFileAllocation},
		{"negative EOF", size(1 << 63), file, smb.StatusInvalidParameter, wire.InfoFile, wire.ClassFileEndOfFile},
		{"allocation past the largest cluster", size(1<<63 - 4095), file, smb.StatusInvalidParameter, wire.InfoFile, wire.ClassFileAllocation},
		{"EOF beyond storage", size(1 << 62), file, smb.StatusFileTooLarge, wire.InfoFile, wire.ClassFileEndOfFile},
		{"short basic", []byte{1}, file, smb.StatusInfoLengthMismatch, wire.InfoFile, wire.ClassFileBasic},
		{"short EOF", []byte{1}, file, smb.StatusInfoLengthMismatch, wire.InfoFile, wire.ClassFileEndOfFile},
		{"hard link", []byte{1}, file, smb.StatusNotSupported, wire.InfoFile, 11},
		{"read-only class", []byte{1}, file, smb.StatusNotSupported, wire.InfoFile, wire.ClassFileStandard},
		{"filesystem", nil, file, smb.StatusNotSupported, wire.InfoFilesystem, 0},
		{"security", nil, file, smb.StatusNotSupported, wire.InfoSecurity, 0},
		{"quota", nil, file, smb.StatusNotSupported, 4, 0},
		{"closed file", size(0), closed, smb.StatusFileClosed, wire.InfoFile, wire.ClassFileEndOfFile},
	} {
		if status := client.setInfo(t, wire.SetInfoRequest{ID: test.id, InfoType: test.infoType, InfoClass: uint8(test.class), Input: test.input}); status != test.want {
			t.Errorf("%s: status %#x, want %#x", test.name, status, test.want)
		}
	}
	if got := endOfFile(t, client, file); got != 7 {
		t.Fatalf("refused requests left EOF %d", got)
	}
}

// Regression for #428: a SET_INFO that waits on storage replies
// STATUS_PENDING and lets the connection serve other requests meanwhile.
func TestSetInfoWaitingOnStorageRepliesAsync(t *testing.T) {
	explicit := toFiletime(t, time.Date(2001, 1, 2, 3, 4, 5, 0, time.UTC))
	data := bytes.Repeat([]byte("123456789"), 1000)
	for _, test := range []struct {
		name  string
		input []byte
		eof   uint64
		class wire.FileInfoClass
	}{
		{"basic", encode(t, wire.EncodeFileBasicInformation, wire.FileBasicInformation{Accessed: explicit, Modified: explicit, Changed: explicit}), 9000, wire.ClassFileBasic},
		{"end of file", encode(t, wire.EncodeFileEndOfFileInformation, wire.FileEndOfFileInformation{EndOfFile: 5000}), 5000, wire.ClassFileEndOfFile},
		{"allocation", encode(t, wire.EncodeFileAllocationInformation, wire.FileAllocationInformation{AllocationSize: 4096}), 4096, wire.ClassFileAllocation},
	} {
		for _, failure := range []error{nil, smb.ErrIO} {
			t.Run(fmt.Sprintf("%s/%v", test.name, failure), func(t *testing.T) {
				srv := newTestServer(t)
				client := srv.connect(t)
				id := client.open(t, "file")
				writeFile(t, client, id, data)
				want, wantEOF := smb.StatusSuccess, test.eof
				if failure != nil {
					want, wantEOF = smb.StatusIODeviceError, uint64(len(data))
				}
				request := wire.SetInfoRequest{ID: id, InfoType: wire.InfoFile, InfoClass: uint8(test.class), Input: test.input}
				if status := setInfoBlockedInStorage(t, srv, client, request, failure); status != want {
					t.Fatalf("final status %#x, want %#x", status, want)
				}
				// A later flush keeps the new size and explicit times.
				if status := client.flush(t, wire.FlushRequest{ID: id}); status != smb.StatusSuccess {
					t.Fatalf("FLUSH status %#x", status)
				}
				if got := endOfFile(t, client, id); got != wantEOF {
					t.Fatalf("EOF = %d, want %d", got, wantEOF)
				}
				if test.class == wire.ClassFileBasic && failure == nil && basicInfo(t, client, id).Modified != explicit {
					t.Fatal("FLUSH overwrote the explicit times")
				}
			})
		}
	}
}

// setInfoBlockedInStorage sends request while storage holds SetAttr. It
// checks the interim reply, that the connection still answers and that
// nothing changed, then lets SetAttr finish with failure, or pass through
// when failure is nil, and returns the final status.
func setInfoBlockedInStorage(t *testing.T, srv *testServer, client *testClient, request wire.SetInfoRequest, failure error) smb.Status {
	t.Helper()
	before := basicInfo(t, client, request.ID)
	entered, release := make(chan struct{}, 1), make(chan struct{})
	srv.faults.set(func(hooks *storageHooks) {
		hooks.SetAttr = func(ctx context.Context, object smb.Inode, change smb.AttrChange) error {
			entered <- struct{}{}
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
			if failure != nil {
				return failure
			}
			return srv.storage.SetAttr(ctx, object, change)
		}
	})
	defer srv.faults.set(func(hooks *storageHooks) { hooks.SetAttr = nil })
	header := client.send(t, wire.SetInfo, encode(t, wire.EncodeSetInfoRequest, request), 1)
	<-entered
	client.interim(t, header)
	client.echo(t)
	if after := basicInfo(t, client, request.ID); after != before {
		t.Fatalf("waiting SET_INFO changed %+v to %+v", before, after)
	}
	close(release)
	return client.receive(t, header).Header.Status
}

// Regression for #428: an allocation hint needs no storage call, so it gets
// one synchronous reply.
func TestSetInfoAllocationHintRepliesAtOnce(t *testing.T) {
	srv := newTestServer(t)
	client := srv.connect(t)
	id := client.open(t, "file")
	writeFile(t, client, id, []byte("buffered data"))
	srv.faults.set(func(hooks *storageHooks) {
		hooks.SetAttr = func(context.Context, smb.Inode, smb.AttrChange) error {
			t.Error("allocation hint reached storage")
			return smb.ErrIO
		}
	})
	body := encode(t, wire.EncodeSetInfoRequest, wire.SetInfoRequest{ID: id, InfoType: wire.InfoFile, InfoClass: uint8(wire.ClassFileAllocation), Input: encode(t, wire.EncodeFileAllocationInformation, wire.FileAllocationInformation{AllocationSize: 4096})})
	request := client.send(t, wire.SetInfo, body, 1)
	if reply := client.next(t, request).Header; reply.Status != smb.StatusSuccess || reply.Flags&wire.FlagAsync != 0 {
		t.Fatalf("reply %+v", reply)
	}
	client.echo(t)
}
