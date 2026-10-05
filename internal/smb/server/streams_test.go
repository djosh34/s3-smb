package server

import (
	"fmt"
	"maps"
	"strings"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

// The streams macOS uses for resource forks, Finder info and xattrs.
var macStreams = []string{"AFP_Resource", "AFP_AfpInfo", "com.apple.FinderInfo", "other.xattr"}

// streamSizes lists the streams of id by name with their sizes. Each stream
// must report an allocation of at least its size.
func streamSizes(t *testing.T, client *testClient, id wire.FileID) map[string]uint64 {
	t.Helper()
	data, status := client.queryInfo(t, wire.QueryInfoRequest{ID: id, InfoType: wire.InfoFile, InfoClass: uint8(wire.ClassFileStream), OutputLength: 4096})
	if status != smb.StatusSuccess {
		t.Fatalf("stream list: status %#x", status)
	}
	listed, err := wire.DecodeFileStreamInformation(data)
	if err != nil {
		t.Fatal(err)
	}
	sizes := make(map[string]uint64)
	for _, entry := range listed.Entries {
		if entry.AllocationSize < entry.Size {
			t.Fatalf("stream %+v allocates less than its size", entry)
		}
		sizes[entry.Name] = entry.Size
	}
	return sizes
}

func expectStreams(t *testing.T, client *testClient, id wire.FileID, want map[string]uint64) {
	t.Helper()
	if got := streamSizes(t, client, id); !maps.Equal(got, want) {
		t.Fatalf("streams %v, want %v", got, want)
	}
}

// expectLength checks the end of file that every size class reports for id.
func expectLength(t *testing.T, client *testClient, id wire.FileID, want uint64) {
	t.Helper()
	standard := queryClass(t, client, id, wire.ClassFileStandard, wire.DecodeFileStandardInformation)
	all := queryClass(t, client, id, wire.ClassFileAll, wire.DecodeFileAllInformation).Standard
	network := queryClass(t, client, id, wire.ClassFileNetworkOpen, wire.DecodeFileNetworkOpenInformation)
	for _, got := range [][2]uint64{{standard.EndOfFile, standard.AllocationSize}, {all.EndOfFile, all.AllocationSize}, {network.EndOfFile, network.AllocationSize}} {
		if got[0] != want || got[1] < want {
			t.Fatalf("end of file %d with allocation %d, want %d", got[0], got[1], want)
		}
	}
	if standard.Directory {
		t.Fatal("stream reported as a directory")
	}
}

type createResult struct {
	status smb.Status
	action uint32
}

// Regression for #94: a stream's disposition depends on whether the stream
// exists, not its base file, and never touches the base file's data.
func TestStreamDispositions(t *testing.T) {
	srv := newTestServer(t)
	client := srv.connect(t)
	notFound, collision := createResult{smb.StatusObjectNameNotFound, 0}, createResult{smb.StatusObjectNameCollision, 0}
	created, opened := createResult{smb.StatusSuccess, 2}, createResult{smb.StatusSuccess, 1}
	// Indexed by disposition: supersede, open, create, open if, overwrite, overwrite if.
	for presence, results := range map[string][6]createResult{
		"no base":   {notFound, notFound, notFound, notFound, notFound, notFound},
		"base only": {created, notFound, created, created, notFound, created},
		"stream":    {{smb.StatusSuccess, 0}, opened, collision, opened, {smb.StatusSuccess, 3}, {smb.StatusSuccess, 3}},
	} {
		for _, stream := range macStreams {
			for disposition, want := range results {
				checkStreamDisposition(t, srv, client, presence, stream, uint32(disposition), want)
			}
		}
	}
}

func checkStreamDisposition(t *testing.T, srv *testServer, client *testClient, presence, stream string, disposition uint32, want createResult) {
	t.Helper()
	base := fmt.Sprintf("%s %s %d", presence, stream, disposition)
	name := base + ":" + stream + ":$DATA"
	if presence != "no base" {
		seed(t, client, base, "base data")
	}
	if presence == "stream" {
		seed(t, client, name, "old stream")
	}
	reply, status := createAs(t, client, fullAccess(name, disposition))
	if got := (createResult{status, reply.Action}); got != want {
		t.Errorf("%s: got %+v, want %+v", name, got, want)
		return
	}
	contents := map[string]string{base: "base data"}
	switch {
	case presence == "no base":
		contents = map[string]string{base: ""}
	case status == smb.StatusObjectNameNotFound:
		contents[name] = ""
	case want.action == 1 || status == smb.StatusObjectNameCollision:
		contents[name] = "old stream"
	default:
		// Created or replaced: the stream exists and is empty.
		if got := srv.content(t, name); got != "" {
			t.Errorf("%s: stream holds %q", name, got)
		}
	}
	srv.expectContent(t, contents)
	if status == smb.StatusSuccess {
		closeOK(t, client, reply.ID)
	}
}

// Streams have their own offsets, holes and length, up to the storage limit.
func TestStreamDataAndSizeLimit(t *testing.T) {
	srv := newTestServer(t)
	client := srv.connect(t)
	seed(t, client, "file", "base data")
	expectRead := func(id wire.FileID, offset uint64, want string) {
		t.Helper()
		if data, status := client.read(t, wire.ReadRequest{ID: id, Offset: offset, Length: 64}); status != smb.StatusSuccess || string(data) != want {
			t.Fatalf("READ at %d = %q, %#x; want %q", offset, data, status, want)
		}
	}
	expectWrite := func(id wire.FileID, offset uint64, data string, want smb.Status) {
		t.Helper()
		if status := client.write(t, wire.WriteRequest{ID: id, Offset: offset, Data: []byte(data)}); status != want {
			t.Fatalf("WRITE at %d: status %#x, want %#x", offset, status, want)
		}
	}
	for _, stream := range macStreams {
		id := client.open(t, "file:"+stream)
		expectWrite(id, 0, "abcdef", smb.StatusSuccess)
		expectWrite(id, 2, "XY", smb.StatusSuccess)
		expectRead(id, 0, "abXYef")
		for _, size := range []uint64{3, 6} {
			if status := setEndOfFile(t, client, id, size); status != smb.StatusSuccess {
				t.Fatalf("EOF %d: status %#x", size, status)
			}
		}
		expectWrite(id, 8, "Z", smb.StatusSuccess)
		expectRead(id, 0, "abX\x00\x00\x00\x00\x00Z")
		expectWrite(id, smb.MaxStreamSize-1, "!", smb.StatusSuccess)
		expectWrite(id, smb.MaxStreamSize, "!", smb.StatusFileTooLarge)
		expectWrite(id, smb.MaxStreamSize-1, "??", smb.StatusFileTooLarge)
		if status := setEndOfFile(t, client, id, smb.MaxStreamSize+1); status != smb.StatusFileTooLarge {
			t.Fatalf("EOF past the limit: status %#x", status)
		}
		expectLength(t, client, id, smb.MaxStreamSize)
		expectRead(id, smb.MaxStreamSize-2, "\x00!")
		closeOK(t, client, id)
	}
	srv.expectContent(t, map[string]string{"file": "base data"})
}

// Regression for #97: size queries and CLOSE on a stream report the stream's
// size, not its base file's.
func TestStreamInformation(t *testing.T) {
	const payload = "stream payload"
	for _, test := range []struct {
		base string
		size uint64
	}{{"", 0}, {"base", 4}, {strings.Repeat("b", 128), 128}} {
		t.Run(fmt.Sprint(test.size), func(t *testing.T) {
			client := newTestServer(t).connect(t)
			base := client.open(t, "data")
			if test.size != 0 {
				writeFile(t, client, base, []byte(test.base))
			}
			want := map[string]uint64{"::$DATA": test.size}
			for _, stream := range macStreams {
				id := client.open(t, "data:"+stream+":$DATA")
				writeFile(t, client, id, []byte(payload))
				expectLength(t, client, id, uint64(len(payload)))
				response, status := decodeReply(t, client.call(t, wire.Close, encode(t, wire.EncodeCloseRequest, wire.CloseRequest{ID: id, Flags: 1}), 1), wire.DecodeCloseResponse)
				if status != smb.StatusSuccess || response.Flags != 1 || response.Size != uint64(len(payload)) {
					t.Fatalf("CLOSE of %s = %+v, %#x", stream, response, status)
				}
				want[":"+stream+":$DATA"] = uint64(len(payload))
			}
			expectStreams(t, client, base, want)
			resource := client.open(t, "data:AFP_Resource")
			expectStreams(t, client, resource, want)
			expectLength(t, client, base, test.size)
		})
	}
}

// With AAPL, an empty stream counts as absent for FILE_OPEN and stream
// lists, as Samba's vfs_fruit does; macOS creates such streams and expects
// them to be invisible. Only a successful AAPL CREATE turns this on, from
// that CREATE on, and only for its own connection.
func TestAAPLHidesEmptyStreams(t *testing.T) {
	srv := newTestServer(t)
	client, other := srv.connect(t), srv.connect(t)
	base := client.open(t, "data")
	for _, stream := range macStreams {
		closeOK(t, client, client.open(t, "data:"+stream))
	}
	populated := client.open(t, "data:populated")
	writeFile(t, client, populated, []byte("x"))
	all := map[string]uint64{"::$DATA": 0, ":populated:$DATA": 1}
	for _, stream := range macStreams {
		all[":"+stream+":$DATA"] = 0
	}
	expectStreams(t, client, base, all)

	missing := fullAccess("missing", fileOpen)
	missing.Contexts = []wire.CreateContext{aaplQuery(t, 2)}
	if _, status := createAs(t, client, missing); status != smb.StatusObjectNameNotFound {
		t.Fatalf("AAPL CREATE of a missing file: status %#x", status)
	}
	closeOK(t, client, mustOpen(t, client, fullAccess("data:AFP_Resource", fileOpen)).ID)
	first := fullAccess("data:AFP_Resource", fileOpen)
	first.Contexts = missing.Contexts
	if _, status := createAs(t, client, first); status != smb.StatusObjectNameNotFound {
		t.Fatalf("empty stream opened by the AAPL CREATE: status %#x", status)
	}
	closeOK(t, client, openRoot(t, client, aaplQuery(t, 2)).ID)

	visible := map[string]uint64{"::$DATA": 0, ":populated:$DATA": 1}
	expectStreams(t, client, base, visible)
	expectStreams(t, client, populated, visible)
	expectStreams(t, other, other.open(t, "data"), all)
	for _, stream := range macStreams {
		name := "data:" + stream
		directory := fullAccess(name, fileOpen)
		directory.Options = fileDirectoryFile
		for _, test := range []struct {
			request wire.CreateRequest
			want    smb.Status
		}{
			{fullAccess(name, fileOpen), smb.StatusObjectNameNotFound},
			{directory, smb.StatusNotADirectory},
			{fullAccess(name, fileCreateDisposition), smb.StatusObjectNameCollision},
		} {
			if _, status := createAs(t, client, test.request); status != test.want {
				t.Errorf("%s options %#x disposition %d: status %#x, want %#x", name, test.request.Options, test.request.Disposition, status, test.want)
			}
		}
		if reply := mustOpen(t, client, fullAccess(name, fileOpenIf)); reply.Action != 1 || reply.Size != 0 {
			t.Errorf("FILE_OPEN_IF of empty %s: %+v", name, reply)
		} else {
			writeFile(t, client, reply.ID, []byte("x"))
			closeOK(t, client, reply.ID)
		}
		closeOK(t, client, mustOpen(t, client, fullAccess(name, fileOpen)).ID)
	}
	closeOK(t, client, mustOpen(t, client, fullAccess("data::$DATA", fileOpen)).ID)
}
