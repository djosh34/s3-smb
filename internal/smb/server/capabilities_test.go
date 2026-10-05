package server

import (
	"slices"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func aaplQuery(t *testing.T, requested uint64) wire.CreateContext {
	t.Helper()
	return createContext(t, wire.EncodeAAPLQuery, wire.AAPLQuery{Requested: requested, ClientCapabilities: ^uint64(0)})
}

// openRoot opens the share root for reading with extra create contexts.
func openRoot(t *testing.T, client *testClient, contexts ...wire.CreateContext) wire.CreateResponse {
	t.Helper()
	return mustOpen(t, client, wire.CreateRequest{DesiredAccess: fileGenericRead, ShareAccess: 7, Disposition: fileOpen, Options: fileDirectoryFile, Contexts: contexts})
}

// macOS sends an AAPL query on the first CREATE of every connection and turns
// on its full-sync FLUSH only when the reply says FULL_SYNC.
func TestAAPLReplyOnEveryConnection(t *testing.T) {
	srv := newTestServer(t)
	clients := []*testClient{srv.connect(t), srv.connect(t)}
	full := wire.AAPLReply{Returned: 7, VolumeCapabilities: 0x06, Model: "s3-smb"} // Case sensitive and full sync.
	for requested, want := range map[uint64]wire.AAPLReply{
		0:          {},
		1:          {Returned: 1},
		2:          {Returned: 2, VolumeCapabilities: 0x06},
		4:          {Returned: 4, Model: "s3-smb"},
		7:          full,
		^uint64(0): full,
	} {
		for _, client := range clients {
			for range 2 {
				reply := openRoot(t, client, aaplQuery(t, requested))
				closeOK(t, client, reply.ID)
				if len(reply.Contexts) != 1 {
					t.Fatalf("requested %#x: contexts %+v", requested, reply.Contexts)
				}
				if got, err := wire.DecodeAAPLReply(reply.Contexts[0]); err != nil || got != want {
					t.Fatalf("requested %#x: reply %+v, %v; want %+v", requested, got, err, want)
				}
			}
		}
	}
}

func TestAAPLInvalidQueryChangesNothing(t *testing.T) {
	srv := newTestServer(t)
	client := srv.connect(t)
	query := aaplQuery(t, 7)
	for _, test := range []struct {
		name     string
		contexts []wire.CreateContext
	}{
		{"empty", []wire.CreateContext{{Name: "AAPL"}}},
		{"short", []wire.CreateContext{{Name: "AAPL", Data: query.Data[:1]}}},
		{"long", []wire.CreateContext{{Name: "AAPL", Data: append(slices.Clone(query.Data), 0)}}},
		{"not a query", []wire.CreateContext{{Name: "AAPL", Data: append([]byte{2}, query.Data[1:]...)}}},
		{"two queries", []wire.CreateContext{query, query}},
	} {
		request := fullAccess("file", fileCreateDisposition)
		request.Contexts = test.contexts
		if _, status := createAs(t, client, request); status != smb.StatusInvalidParameter {
			t.Errorf("%s: status %#x", test.name, status)
		}
	}
	srv.expectContent(t, map[string]string{"file": ""})
	if reply := openRoot(t, client, wire.CreateContext{Name: "unknown"}); len(reply.Contexts) != 0 {
		t.Fatalf("unknown context answered %+v", reply.Contexts)
	}
}

// The server refuses every IOCTL. The five connection-wide ones need no
// open; the others name an open, which must exist.
func TestIOCTLRefused(t *testing.T) {
	client := newTestServer(t).connect(t)
	file, root := client.open(t, "file"), openRoot(t, client).ID
	everything := wire.FileID{Persistent: ^uint64(0), Volatile: ^uint64(0)}
	closed := file
	closed.Volatile++
	for _, test := range []struct {
		name  string
		id    wire.FileID
		code  uint32
		flags uint32
		want  smb.Status
	}{
		{"DFS referrals", everything, 0x00060194, 1, smb.StatusNotSupported},
		{"DFS referrals ex", everything, 0x000601b0, 1, smb.StatusNotSupported},
		{"pipe wait", everything, 0x00110018, 1, smb.StatusNotSupported},
		{"network interfaces", everything, 0x001401fc, 1, smb.StatusNotSupported},
		{"validate negotiate", everything, 0x00140204, 1, smb.StatusNotSupported},
		{"not an FSCTL", everything, 0x000900c0, 0, smb.StatusNotSupported},
		{"object ID of a file", file, 0x000900c0, 1, smb.StatusNotSupported},
		{"object ID of the root", root, 0x000900c0, 1, smb.StatusNotSupported},
		{"set sparse", file, 0x000900c4, 1, smb.StatusNotSupported},
		{"resume key for server-side copy", file, 0x00140078, 1, smb.StatusNotSupported},
		{"object ID of no open", everything, 0x000900c0, 1, smb.StatusFileClosed},
		{"closed open", closed, 0x000900c0, 1, smb.StatusFileClosed},
	} {
		if status := client.ioctl(t, wire.IOCTLRequest{ID: test.id, ControlCode: test.code, Flags: test.flags, MaxOutput: 1024}); status != test.want {
			t.Errorf("%s: status %#x, want %#x", test.name, status, test.want)
		}
	}
	body := encode(t, wire.EncodeIOCTLRequest, wire.IOCTLRequest{ID: file, ControlCode: 0x000900c0, Flags: 1})
	if status := client.call(t, wire.IOCTL, body[:55], 1).Header.Status; status != smb.StatusInvalidParameter {
		t.Errorf("malformed IOCTL: status %#x", status)
	}
	if attributes := basicInfo(t, client, file).Attributes; attributes&0x200 != 0 {
		t.Errorf("refused FSCTL_SET_SPARSE left attributes %#x", attributes)
	}
	closeOK(t, client, file)
}

// Names are case sensitive, keep their case and store any Unicode, as the
// filesystem attributes advertise. Named streams are advertised too and tested
// in streams_test.go. Nothing else is advertised: no sparse files, hard links
// or object IDs.
func TestFilesystemNames(t *testing.T) {
	client := newTestServer(t).connect(t)
	root := openRoot(t, client).ID
	data, status := queryFilesystem(t, client, root, wire.ClassFilesystemAttribute, 1024)
	if attributes, err := wire.DecodeFilesystemAttributeInformation(data); status != smb.StatusSuccess || err != nil || attributes.Attributes != 0x40007 {
		t.Fatalf("filesystem attributes %+v, %#x, %v", attributes, status, err)
	}
	unicode := "資料-😀-café"
	for _, name := range []string{"MixedCase", "mixedcase", unicode} {
		closeOK(t, client, mustOpen(t, client, fullAccess(name, fileCreateDisposition)).ID)
	}
	id := mustOpen(t, client, fullAccess("MixedCase", fileOpen)).ID
	if renamed := rename(t, client, id, "MIXEDCASE", false); renamed != smb.StatusSuccess {
		t.Fatalf("rename to another case: status %#x", renamed)
	}
	if name := queryClass(t, client, id, wire.ClassFileName, wire.DecodeFileNameInformation); name.Name != "\\MIXEDCASE" {
		t.Fatalf("name after rename %q", name.Name)
	}
	closeOK(t, client, id)
	if _, opened := createAs(t, client, fullAccess("MixedCase", fileOpen)); opened != smb.StatusObjectNameNotFound {
		t.Fatalf("open of the old case: status %#x", opened)
	}
	names, status := listNames(t, client, root, "*", 0)
	if want := []string{".", "..", "MIXEDCASE", "mixedcase", unicode}; status != smb.StatusSuccess || !sameNames(names, want) {
		t.Fatalf("root lists %q, %#x; want %q", names, status, want)
	}
}

func sameNames(got, want []string) bool {
	got, want = slices.Clone(got), slices.Clone(want)
	slices.Sort(got)
	slices.Sort(want)
	return slices.Equal(got, want)
}
