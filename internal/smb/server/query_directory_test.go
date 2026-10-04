package server

import (
	"context"
	"fmt"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/smbtest"
	"github.com/djosh34/s3-smb/internal/smb/state"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

type directoryFixture struct {
	server  *Server
	client  *smbtest.Client
	ctx     context.Context
	session smbtest.Session
	next    uint64
}

func newDirectoryFixture(t *testing.T) *directoryFixture {
	t.Helper()
	options := testOptions(t)
	options.Storage = newFilesMetaStorage(t)
	server, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	local, remote := net.Pipe()
	client, err := smbtest.NewClient(remote)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- server.ServeConn(ctx, local) }()
	t.Cleanup(func() {
		if closeErr := client.Close(); closeErr != nil {
			t.Error(closeErr)
		}
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("ServeConn did not stop")
		}
		if shutdownErr := server.Shutdown(context.WithoutCancel(t.Context())); shutdownErr != nil {
			t.Error(shutdownErr)
		}
	})
	session, err := client.Login(ctx, smbtest.LoginOptions{Share: options.ShareName, Account: options.Account, Cipher: smb.CipherAES256GCM, Signing: smb.SigningGMAC, ClientGUID: [16]byte{2}})
	if err != nil {
		t.Fatal(err)
	}
	return &directoryFixture{server: server, client: client, ctx: ctx, session: session, next: session.NextMessageID}
}

func (f *directoryFixture) create(t *testing.T, name string, kind smb.Kind) smb.Resolved {
	t.Helper()
	resolved, err := f.server.options.Storage.Lookup(f.ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err = f.server.options.Storage.Create(f.ctx, resolved.Name, kind)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func (f *directoryFixture) open(t *testing.T, name string, access uint32) state.Open {
	t.Helper()
	storage := f.server.options.Storage
	resolved, err := storage.Lookup(f.ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	reservation, status := f.server.options.State.Reserve(state.OpenRequest{Object: resolved.Object, Binding: state.Binding{SessionID: f.session.SessionID, TreeID: f.session.TreeID}, User: f.server.options.Account.User, Share: f.server.options.ShareName, GrantedAccess: access, Sharing: 7})
	if status != smb.StatusSuccess {
		t.Fatal(status)
	}
	handle, err := storage.Open(f.ctx, resolved.Object, smb.AccessRead|smb.AccessWrite)
	if err != nil {
		t.Fatal(err)
	}
	open, status := f.server.options.State.Commit(reservation, state.Grant{Handle: handle, Directory: resolved.Attr.Kind == smb.KindDirectory})
	if status != smb.StatusSuccess {
		t.Fatal(status)
	}
	return open
}

func (f *directoryFixture) query(t *testing.T, open state.Open, pattern string, flags uint8, class wire.DirectoryInfoClass, length uint32) (smb.Status, []wire.DirectoryEntry) {
	t.Helper()
	query := wire.QueryDirectoryRequest{ID: wire.FileID(open.ID), Pattern: pattern, Flags: flags, InfoClass: class, OutputLength: length, FileIndex: 0xffffffff}
	body, err := wire.EncodeQueryDirectoryRequest(query)
	if err != nil {
		t.Fatal(err)
	}
	units := (uint64(length) + uint64(smb.CreditUnit) - 1) / uint64(smb.CreditUnit)
	if units > 65535 {
		t.Fatal("output length exceeds the credit charge field")
		return smb.StatusInvalidParameter, nil
	}
	charge := max(uint16(1), uint16(units))
	message := wire.Message{Header: wire.Header{Command: wire.QueryDirectory, MessageID: f.next, SessionID: f.session.SessionID, TreeID: f.session.TreeID, CreditCharge: charge, Credit: 16}, Body: body}
	f.next += uint64(charge)
	response := exchange(f.ctx, t, f.client, message)[0]
	if response.Header.Status != smb.StatusSuccess {
		return response.Header.Status, nil
	}
	buffer, err := wire.DecodeQueryDirectoryResponse(response)
	if err != nil {
		t.Fatal(err)
	}
	if len(buffer.Data) > int(length) {
		t.Fatalf("reply length %d exceeds %d", len(buffer.Data), length)
	}
	entries := decodeDirectoryTestEntries(t, class, buffer.Data)
	for _, entry := range entries {
		if entry.Metadata.FileIndex != 0 {
			t.Fatal("server advertised a directory index")
		}
	}
	return response.Header.Status, entries
}

func decodeDirectoryTestEntries(t *testing.T, class wire.DirectoryInfoClass, data []byte) []wire.DirectoryEntry {
	t.Helper()
	var entries []wire.DirectoryEntry
	var err error
	switch uint8(class) {
	case uint8(wire.ClassDirectory):
		entries, err = wire.DecodeDirectoryEntries(data)
	case uint8(wire.ClassDirectoryFull):
		entries, err = wire.DecodeDirectoryFullEntries(data)
	case uint8(wire.ClassDirectoryBoth):
		entries, err = wire.DecodeDirectoryBothEntries(data)
	case uint8(wire.ClassDirectoryNames):
		entries, err = wire.DecodeDirectoryNamesEntries(data)
	case uint8(wire.ClassDirectoryIDBoth):
		var decoded []wire.DirectoryIDBothEntry
		decoded, err = wire.DecodeDirectoryIDBothEntries(data)
		for _, entry := range decoded {
			entries = append(entries, wire.DirectoryEntry(entry))
		}
	case uint8(wire.ClassDirectoryIDFull):
		var decoded []wire.DirectoryIDFullEntry
		decoded, err = wire.DecodeDirectoryIDFullEntries(data)
		for _, entry := range decoded {
			entries = append(entries, wire.DirectoryEntry{Name: entry.Name, Metadata: entry.Metadata})
		}
	default:
		t.Fatalf("unsupported test class %d", class)
	}
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

func directoryNames(entries []wire.DirectoryEntry) []string {
	names := make([]string, len(entries))
	for i, entry := range entries {
		names[i] = entry.Name
	}
	return names
}

func TestDirectoryContinuationReusesOriginalPattern(t *testing.T) {
	f := newDirectoryFixture(t)
	f.create(t, "alpha", smb.KindFile)
	f.create(t, "beta", smb.KindFile)
	open := f.open(t, "", 1)
	for i, want := range []string{"alpha", "beta"} {
		pattern, flags := "", uint8(directorySingle)
		if i == 0 {
			pattern, flags = "*", directoryRestart|directorySingle
		}
		status, entries := f.query(t, open, pattern, flags, wire.ClassDirectoryIDBoth, 4096)
		if status != smb.StatusSuccess || !reflect.DeepEqual(directoryNames(entries), []string{want}) {
			t.Fatalf("query %d: %#x, %v", i, status, directoryNames(entries))
		}
	}
	for range 2 {
		status, _ := f.query(t, open, "", directorySingle, wire.ClassDirectoryIDBoth, 4096)
		if status != smb.StatusNoMoreFiles {
			t.Fatalf("exhausted: %#x", status)
		}
	}
}

func TestDirectoryEntriesReportLiveLength(t *testing.T) {
	f := newDirectoryFixture(t)
	object := f.create(t, "buffered", smb.KindFile)
	writer := f.open(t, "buffered", 3)
	if n, err := f.server.options.Storage.WriteAt(f.ctx, writer.Handle, []byte("live data"), 101); err != nil || n != 9 {
		t.Fatalf("write = %d, %v", n, err)
	}
	open := f.open(t, "", 1)
	for _, class := range []wire.DirectoryInfoClass{wire.ClassDirectory, wire.ClassDirectoryFull, wire.ClassDirectoryBoth, wire.ClassDirectoryNames, wire.ClassDirectoryIDBoth, wire.ClassDirectoryIDFull} {
		status, entries := f.query(t, open, "buffered", directoryReopen, class, 4096)
		if status != smb.StatusSuccess || len(entries) != 1 || entries[0].Name != "buffered" {
			t.Fatalf("class %d: %#x, %+v", class, status, entries)
		}
		if class != wire.ClassDirectoryNames && entries[0].Metadata.EndOfFile != 110 {
			t.Fatalf("class %d stale length: %+v", class, entries[0])
		}
		if (class == wire.ClassDirectoryIDBoth || class == wire.ClassDirectoryIDFull) && entries[0].Metadata.FileID != uint64(object.Object.Inode) {
			t.Fatalf("class %d wrong inode: %+v", class, entries[0])
		}
	}
}

func TestDirectoryPagingListsEveryNameOnce(t *testing.T) {
	f := newDirectoryFixture(t)
	want := make(map[string]bool)
	for i := range 150 {
		name := fmt.Sprintf("entry-%03d", i)
		f.create(t, name, smb.KindFile)
		want[name] = true
	}
	open := f.open(t, "", 1)
	seen := make(map[string]bool)
	for page := range 151 {
		pattern := ""
		if page == 0 {
			pattern = "*"
		}
		status, entries := f.query(t, open, pattern, 0, wire.ClassDirectoryIDFull, 210)
		if status == smb.StatusNoMoreFiles {
			break
		}
		if status != smb.StatusSuccess || len(entries) == 0 {
			t.Fatalf("page %d: %#x", page, status)
		}
		for _, entry := range entries {
			if seen[entry.Name] || !want[entry.Name] {
				t.Fatalf("duplicate or unexpected entry %q", entry.Name)
			}
			seen[entry.Name] = true
		}
	}
	if !reflect.DeepEqual(seen, want) {
		t.Fatalf("listed %d of %d entries", len(seen), len(want))
	}
	status, entries := f.query(t, open, "entry-149", directoryReopen, wire.ClassDirectoryNames, 4096)
	if status != smb.StatusSuccess || !reflect.DeepEqual(directoryNames(entries), []string{"entry-149"}) {
		t.Fatalf("filter across storage pages: %#x, %+v", status, entries)
	}
}

func TestDirectoryRestartAndReopen(t *testing.T) {
	f := newDirectoryFixture(t)
	f.create(t, "alpha", smb.KindFile)
	f.create(t, "beta", smb.KindFile)
	open := f.open(t, "", 1)
	cases := []struct {
		pattern string
		want    string
		flags   uint8
	}{
		{"*", "alpha", directorySingle},
		{"", "alpha", directoryRestart | directorySingle},
		{"beta", "beta", directorySingle},
		{"alpha", "alpha", directoryReopen | directorySingle},
		{"", "alpha", directoryRestart | directorySingle},
		{"beta", "beta", directoryReopen | directorySingle},
	}
	for _, test := range cases {
		status, entries := f.query(t, open, test.pattern, test.flags, wire.ClassDirectoryNames, 4096)
		if status != smb.StatusSuccess || !reflect.DeepEqual(directoryNames(entries), []string{test.want}) {
			t.Fatalf("%+v: %#x, %v", test, status, directoryNames(entries))
		}
	}
	status, _ := f.query(t, open, "", 0, wire.ClassDirectoryNames, 4096)
	if status != smb.StatusNoMoreFiles {
		t.Fatalf("exact search continued: %#x", status)
	}
}

func TestDirectoryDotsAndSmallBuffer(t *testing.T) {
	f := newDirectoryFixture(t)
	parent := f.create(t, "parent", smb.KindDirectory)
	child := f.create(t, "parent/child", smb.KindDirectory)
	f.create(t, "parent/child/alpha", smb.KindFile)
	open := f.open(t, "parent/child", 1)
	smallStatus, _ := f.query(t, open, "*", directoryRestart, wire.ClassDirectoryIDBoth, 1)
	if smallStatus != smb.StatusBufferTooSmall {
		t.Fatalf("small buffer: %#x", smallStatus)
	}
	for i, want := range []string{".", "..", "alpha"} {
		status, entries := f.query(t, open, "", directorySingle, wire.ClassDirectoryIDBoth, 120)
		if status != smb.StatusSuccess || !reflect.DeepEqual(directoryNames(entries), []string{want}) {
			t.Fatalf("entry %d: %#x, %+v", i, status, entries)
		}
		if i < 2 {
			inode := child.Object.Inode
			if i == 1 {
				inode = parent.Object.Inode
			}
			if entries[0].Metadata.FileID != uint64(inode) {
				t.Fatalf("dot inode: %+v", entries[0])
			}
		}
	}
	status, entries := f.query(t, open, "", directoryRestart, wire.ClassDirectoryNames, 4096)
	if status != smb.StatusSuccess || !reflect.DeepEqual(directoryNames(entries), []string{".", "..", "alpha"}) {
		t.Fatalf("restart dots: %#x, %+v", status, entries)
	}
}

func TestDirectoryStatusRepliesAndEcho(t *testing.T) {
	f := newDirectoryFixture(t)
	f.create(t, "alpha", smb.KindFile)
	root := f.open(t, "", 1)
	file := f.open(t, "alpha", 1)
	denied := f.open(t, "", 0x80)
	closed := root
	closed.ID.Volatile++
	cases := []struct {
		open    *state.Open
		pattern string
		length  uint32
		want    smb.Status
		class   wire.DirectoryInfoClass
		flags   uint8
	}{
		{&root, "*", 4096, smb.StatusInvalidInfoClass, 255, 0},
		{&root, strings.Repeat("a", 256), 4096, smb.StatusObjectNameInvalid, wire.ClassDirectoryNames, 0},
		{&root, strings.Repeat("😀", 128), 4096, smb.StatusObjectNameInvalid, wire.ClassDirectoryNames, 0},
		{&file, "*", 4096, smb.StatusInvalidParameter, wire.ClassDirectoryNames, 0},
		{&denied, "*", 4096, smb.StatusAccessDenied, wire.ClassDirectoryNames, 0},
		{&closed, "*", 4096, smb.StatusFileClosed, wire.ClassDirectoryNames, 0},
		{&root, "missing", 4096, smb.StatusNoSuchFile, wire.ClassDirectoryNames, directoryReopen},
		{&root, "", 4096, smb.StatusNoMoreFiles, wire.ClassDirectoryNames, 0},
		{&root, "*", 0, smb.StatusBufferTooSmall, wire.ClassDirectoryNames, directoryReopen},
		{&root, "*", 4096, smb.StatusInvalidParameter, wire.ClassDirectoryNames, 0x80},
		{&root, "*", smb.MaxTransactSize + 1, smb.StatusInvalidParameter, wire.ClassDirectoryNames, 0},
	}
	for _, test := range cases {
		status, _ := f.query(t, *test.open, test.pattern, test.flags, test.class, test.length)
		if status != test.want {
			t.Fatalf("%+v: %#x", test, status)
		}
		response := exchange(f.ctx, t, f.client, sessionEcho(t, f.session, f.next))[0]
		f.next++
		if response.Header.Status != smb.StatusSuccess {
			t.Fatal("status reply broke ECHO")
		}
	}
}

func TestDirectoryPatternBelongsToEachOpen(t *testing.T) {
	f := newDirectoryFixture(t)
	f.create(t, "alpha", smb.KindFile)
	f.create(t, "beta", smb.KindFile)
	alpha := f.open(t, "", 1)
	beta := f.open(t, "", 1)
	for _, test := range []struct {
		pattern string
		open    state.Open
	}{
		{"alpha", alpha},
		{"beta", beta},
	} {
		status, entries := f.query(t, test.open, test.pattern, directorySingle, wire.ClassDirectoryNames, 4096)
		if status != smb.StatusSuccess || !reflect.DeepEqual(directoryNames(entries), []string{test.pattern}) {
			t.Fatalf("first search %q: %#x, %+v", test.pattern, status, entries)
		}
		status, _ = f.query(t, test.open, "*", directorySingle, wire.ClassDirectoryNames, 4096)
		if status != smb.StatusNoMoreFiles {
			t.Fatalf("continuation replaced pattern %q: %#x", test.pattern, status)
		}
	}
	for _, test := range []struct {
		pattern string
		open    state.Open
	}{{"alpha", alpha}, {"beta", beta}} {
		status, entries := f.query(t, test.open, "", directoryRestart, wire.ClassDirectoryNames, 4096)
		if status != smb.StatusSuccess || !reflect.DeepEqual(directoryNames(entries), []string{test.pattern}) {
			t.Fatalf("restart %q: %#x, %+v", test.pattern, status, entries)
		}
	}
}

func TestDirectoryAcceptsMaximumPatternLength(t *testing.T) {
	f := newDirectoryFixture(t)
	name := strings.Repeat("a", 255)
	f.create(t, name, smb.KindFile)
	open := f.open(t, "", 1)
	status, entries := f.query(t, open, name, 0, wire.ClassDirectoryNames, 4096)
	if status != smb.StatusSuccess || !reflect.DeepEqual(directoryNames(entries), []string{name}) {
		t.Fatalf("255-unit pattern: %#x, %+v", status, entries)
	}
}

func TestDirectoryLiteralBracketsInPattern(t *testing.T) {
	f := newDirectoryFixture(t)
	f.create(t, "part[1].txt", smb.KindFile)
	f.create(t, "part1.txt", smb.KindFile)
	open := f.open(t, "", 1)
	status, entries := f.query(t, open, "part[1]*", directoryRestart, wire.ClassDirectoryNames, 4096)
	if status != smb.StatusSuccess || !reflect.DeepEqual(directoryNames(entries), []string{"part[1].txt"}) {
		t.Fatalf("literal brackets: %#x, %+v", status, entries)
	}
}

func TestDirectoryIgnoresIndexSpecified(t *testing.T) {
	f := newDirectoryFixture(t)
	f.create(t, "alpha", smb.KindFile)
	f.create(t, "beta", smb.KindFile)
	open := f.open(t, "", 1)
	for i, want := range []string{"alpha", "beta"} {
		status, entries := f.query(t, open, "*", directoryIndex|directorySingle, wire.ClassDirectoryNames, 4096)
		if status != smb.StatusSuccess || !reflect.DeepEqual(directoryNames(entries), []string{want}) {
			t.Fatalf("query %d index changed position: %#x, %+v", i, status, entries)
		}
	}
}
