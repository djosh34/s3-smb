package server

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

var directoryClasses = []wire.DirectoryInfoClass{
	wire.ClassDirectory, wire.ClassDirectoryFull, wire.ClassDirectoryBoth,
	wire.ClassDirectoryNames, wire.ClassDirectoryIDBoth, wire.ClassDirectoryIDFull,
}

// openDirectory opens or creates the directory name for listing.
func openDirectory(t *testing.T, client *testClient, name string) wire.FileID {
	t.Helper()
	return openAs(t, client, name, fileGenericRead, 7, fileDirectoryFile)
}

// list sends one QUERY_DIRECTORY and decodes the entries of a successful reply.
func list(t *testing.T, client *testClient, request wire.QueryDirectoryRequest) ([]wire.DirectoryEntry, smb.Status) {
	t.Helper()
	data, status := client.queryDirectory(t, request)
	if status != smb.StatusSuccess {
		return nil, status
	}
	if len(data) > int(request.OutputLength) {
		t.Fatalf("reply of %d bytes for a %d byte buffer", len(data), request.OutputLength)
	}
	var entries []wire.DirectoryEntry
	var err error
	switch uint8(request.InfoClass) {
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
		t.Fatalf("no decoder for class %d", request.InfoClass)
	}
	if err != nil {
		t.Fatal(err)
	}
	return entries, status
}

// listNames lists with the names class and returns only the names.
func listNames(t *testing.T, client *testClient, id wire.FileID, pattern string, flags uint8) ([]string, smb.Status) {
	t.Helper()
	entries, status := list(t, client, wire.QueryDirectoryRequest{ID: id, Pattern: pattern, Flags: flags, InfoClass: wire.ClassDirectoryNames, OutputLength: 4096})
	names := make([]string, len(entries))
	for i, entry := range entries {
		names[i] = entry.Name
	}
	return names, status
}

func TestQueryDirectoryEmptyRoot(t *testing.T) {
	srv := newTestServer(t)
	client := srv.connect(t)
	root := openDirectory(t, client, "")
	inode := uint64(srv.object(t, ""))
	for _, class := range directoryClasses {
		request := wire.QueryDirectoryRequest{ID: root, Pattern: "*", Flags: directoryReopen, InfoClass: class, OutputLength: 4096}
		entries, status := list(t, client, request)
		if status != smb.StatusSuccess || len(entries) != 2 || entries[0].Name != "." || entries[1].Name != ".." {
			t.Fatalf("class %d: %+v, %#x", class, entries, status)
		}
		for _, entry := range entries {
			if class != wire.ClassDirectoryNames && entry.Metadata.Basic.Attributes&0x10 == 0 {
				t.Fatalf("class %d: %q is not a directory", class, entry.Name)
			}
			// The share root is its own parent, so ".." never leaves the share.
			if (class == wire.ClassDirectoryIDBoth || class == wire.ClassDirectoryIDFull) && entry.Metadata.FileID != inode {
				t.Fatalf("class %d: %q has file ID %d, want %d", class, entry.Name, entry.Metadata.FileID, inode)
			}
		}
		request.Pattern, request.Flags = "", 0
		if _, status = list(t, client, request); status != smb.StatusNoMoreFiles {
			t.Fatalf("class %d continuation: status %#x", class, status)
		}
	}
	if _, status := listNames(t, client, root, "missing", directoryReopen); status != smb.StatusNoSuchFile {
		t.Fatalf("unmatched first search: status %#x", status)
	}
	if _, status := listNames(t, client, root, "", 0); status != smb.StatusNoMoreFiles {
		t.Fatalf("unmatched continuation: status %#x", status)
	}
}

// Small buffers page through more names than one storage read returns.
func TestQueryDirectoryPaging(t *testing.T) {
	client := newTestServer(t).connect(t)
	want := []string{".", ".."}
	for i := range 150 {
		name := fmt.Sprintf("entry-%03d", i)
		closeOK(t, client, client.open(t, name))
		want = append(want, name)
	}
	directory := openDirectory(t, client, "")
	var seen []string
	flags := uint8(directoryReopen)
	for page := 0; ; page++ {
		entries, status := list(t, client, wire.QueryDirectoryRequest{ID: directory, Pattern: "*", Flags: flags, InfoClass: wire.ClassDirectoryIDFull, OutputLength: 210})
		flags = 0
		if status == smb.StatusNoMoreFiles {
			break
		}
		if status != smb.StatusSuccess || len(entries) == 0 || page > len(want) {
			t.Fatalf("page %d: %d entries, status %#x", page, len(entries), status)
		}
		for _, entry := range entries {
			seen = append(seen, entry.Name)
		}
	}
	slices.Sort(seen)
	if !slices.Equal(seen, want) {
		t.Fatalf("listed %d names, want %d", len(seen), len(want))
	}
	if names, status := listNames(t, client, directory, "entry-149", directoryReopen); status != smb.StatusSuccess || !slices.Equal(names, []string{"entry-149"}) {
		t.Fatalf("search past the first storage page = %v, %#x", names, status)
	}
}

// RESTART rewinds and keeps the pattern; REOPEN rewinds with a new one. A
// later request's pattern does not replace the search's.
func TestQueryDirectoryRestartAndReopen(t *testing.T) {
	client := newTestServer(t).connect(t)
	closeOK(t, client, client.open(t, "alpha"))
	closeOK(t, client, client.open(t, "beta"))
	directory := openDirectory(t, client, "")
	for i, step := range []struct {
		pattern, want string
		flags         uint8
	}{
		{"*", ".", directorySingle},
		{"", ".", directoryRestart | directorySingle},
		{"alpha", "..", directorySingle},
		{"alpha", "alpha", directoryReopen | directorySingle},
		{"", "alpha", directoryRestart | directorySingle},
		{"beta", "beta", directoryReopen | directorySingle},
		{"", ".", directoryReopen | directorySingle},
		{"", "..", directorySingle},
		{"", "alpha", directorySingle},
		{"", "beta", directorySingle},
	} {
		if names, status := listNames(t, client, directory, step.pattern, step.flags); status != smb.StatusSuccess || !slices.Equal(names, []string{step.want}) {
			t.Fatalf("step %d: %v, %#x; want %q", i, names, status, step.want)
		}
	}
	if _, status := listNames(t, client, directory, "", 0); status != smb.StatusNoMoreFiles {
		t.Fatalf("exhausted search: status %#x", status)
	}
}

func TestQueryDirectoryDotsAndSmallBuffer(t *testing.T) {
	srv := newTestServer(t)
	client := srv.connect(t)
	openDirectory(t, client, "parent")
	child := openDirectory(t, client, "parent/child")
	closeOK(t, client, client.open(t, "parent/child/alpha"))
	request := wire.QueryDirectoryRequest{ID: child, Pattern: "*", Flags: directoryRestart, InfoClass: wire.ClassDirectoryIDBoth, OutputLength: 1}
	if _, status := list(t, client, request); status != smb.StatusBufferTooSmall {
		t.Fatalf("1 byte buffer: status %#x", status)
	}
	request.Pattern, request.Flags, request.OutputLength = "", directorySingle, 120
	for _, want := range []struct {
		name   string
		object string
	}{{".", "parent/child"}, {"..", "parent"}, {"alpha", "parent/child/alpha"}} {
		entries, status := list(t, client, request)
		if status != smb.StatusSuccess || len(entries) != 1 || entries[0].Name != want.name || entries[0].Metadata.FileID != uint64(srv.object(t, want.object)) {
			t.Fatalf("want %q: %+v, %#x", want.name, entries, status)
		}
	}
	if names, status := listNames(t, client, child, "", directoryRestart); status != smb.StatusSuccess || !slices.Equal(names, []string{".", "..", "alpha"}) {
		t.Fatalf("restart = %v, %#x", names, status)
	}
}

// Entries carry the length of data still buffered in an open file.
func TestQueryDirectoryEntriesShowLiveLength(t *testing.T) {
	srv := newTestServer(t)
	client := srv.connect(t)
	id := client.open(t, "buffered")
	if status := client.write(t, wire.WriteRequest{ID: id, Offset: 101, Data: []byte("live data")}); status != smb.StatusSuccess {
		t.Fatalf("WRITE status %#x", status)
	}
	inode := uint64(srv.object(t, "buffered"))
	directory := openDirectory(t, client, "")
	for _, class := range directoryClasses {
		entries, status := list(t, client, wire.QueryDirectoryRequest{ID: directory, Pattern: "buffered", Flags: directoryReopen, InfoClass: class, OutputLength: 4096})
		if status != smb.StatusSuccess || len(entries) != 1 {
			t.Fatalf("class %d: %+v, %#x", class, entries, status)
		}
		metadata := entries[0].Metadata
		if class != wire.ClassDirectoryNames && metadata.EndOfFile != 110 || metadata.FileIndex != 0 {
			t.Fatalf("class %d: %+v", class, metadata)
		}
		if (class == wire.ClassDirectoryIDBoth || class == wire.ClassDirectoryIDFull) && metadata.FileID != inode {
			t.Fatalf("class %d: file ID %d, want %d", class, metadata.FileID, inode)
		}
	}
}

func TestQueryDirectoryPatterns(t *testing.T) {
	client := newTestServer(t).connect(t)
	directory := openDirectory(t, client, "directory")
	for _, name := range []string{"alpha.txt", "part[1].txt", "part1.txt"} {
		closeOK(t, client, client.open(t, "directory/"+name))
	}
	for _, test := range []struct {
		pattern string
		want    []string
	}{
		{"*", []string{".", "..", "alpha.txt", "part1.txt", "part[1].txt"}},
		{"*.*", []string{".", "..", "alpha.txt", "part1.txt", "part[1].txt"}},
		{".*", []string{".", ".."}},
		{"..", []string{".."}},
		{"alpha*", []string{"alpha.txt"}},
		{"part[1]*", []string{"part[1].txt"}},
		{"<.txt", []string{"alpha.txt", "part1.txt", "part[1].txt"}},
	} {
		names, status := listNames(t, client, directory, test.pattern, directoryReopen)
		slices.Sort(names)
		if status != smb.StatusSuccess || !slices.Equal(names, test.want) {
			t.Errorf("%q: %v, %#x", test.pattern, names, status)
		}
	}
}

// matchPattern follows MS-FSA wildcards, compares case-sensitively and
// treats every other character literally.
func TestMatchPattern(t *testing.T) {
	for _, test := range []struct {
		pattern, name string
		want          bool
	}{
		{"", "report.txt", true},
		{"*", "", false},
		{"*.*", "report", true},
		{"*.txt", "reportXtxt", false},
		{"Report.txt", "report.txt", false},
		{"a[1](b)+^$|\\.*", "a[1](b)+^$|\\.txt", true},
		{"re*port", "read-report", true},
		{"*a*b*c*", "0a1c2b3", false},
		{"a?c", "a.c", true},
		{"a?", "a", false},
		{"?", "😀", false},
		{"??", "😀", true},
		{"<", "report.txt", false},
		{"<.txt", "report.old.txt", true},
		{"<.", "report...", true},
		{"a>>>.txt", "a.txt", true},
		{"a>>>.txt", "abcde.txt", false},
		{"a>", "a.", false},
		{"a\"txt", "a.txt", true},
		{"a\"", "a", true},
		{"a\"txt", "atxt", false},
		{"<\">>>", "report.txt", true},
		{"<\">>>", "report.text", false},
	} {
		if got := matchPattern(test.pattern, test.name); got != test.want {
			t.Errorf("matchPattern(%q, %q) = %t", test.pattern, test.name, got)
		}
	}
}

func TestQueryDirectoryRefusals(t *testing.T) {
	client := newTestServer(t).connect(t)
	file := client.open(t, "file")
	directory := openDirectory(t, client, "")
	noList := openAs(t, client, "", 0x80, 7, fileDirectoryFile)
	closed := directory
	closed.Volatile++
	for _, test := range []struct {
		name    string
		request wire.QueryDirectoryRequest
		want    smb.Status
	}{
		{"unknown class", wire.QueryDirectoryRequest{ID: directory, InfoClass: 255}, smb.StatusInvalidInfoClass},
		{"extended ID class", wire.QueryDirectoryRequest{ID: directory, InfoClass: 0x3c}, smb.StatusNotSupported},
		{"longest pattern", wire.QueryDirectoryRequest{ID: directory, Pattern: strings.Repeat("a", 255), Flags: directoryReopen}, smb.StatusNoSuchFile},
		{"pattern too long", wire.QueryDirectoryRequest{ID: directory, Pattern: strings.Repeat("😀", 128)}, smb.StatusObjectNameInvalid},
		{"unknown flag", wire.QueryDirectoryRequest{ID: directory, Flags: 0x80}, smb.StatusInvalidParameter},
		{"buffer too large", wire.QueryDirectoryRequest{ID: directory, OutputLength: smb.MaxTransactSize + 1}, smb.StatusInvalidParameter},
		{"file", wire.QueryDirectoryRequest{ID: file}, smb.StatusInvalidParameter},
		{"no list access", wire.QueryDirectoryRequest{ID: noList}, smb.StatusAccessDenied},
		{"closed", wire.QueryDirectoryRequest{ID: closed}, smb.StatusFileClosed},
	} {
		request := test.request
		request.Pattern = cmp.Or(request.Pattern, "*")
		request.InfoClass = cmp.Or(request.InfoClass, wire.ClassDirectoryNames)
		request.OutputLength = cmp.Or(request.OutputLength, 4096)
		if _, status := list(t, client, request); status != test.want {
			t.Errorf("%s: status %#x, want %#x", test.name, status, test.want)
		}
	}
	client.echo(t)
}

// CHANGE_NOTIFY is refused, so macOS polls directories instead.
func TestChangeNotifyNotSupported(t *testing.T) {
	client := newTestServer(t).connect(t)
	directory := openDirectory(t, client, "")
	body := encode(t, wire.EncodeChangeNotifyRequest, wire.ChangeNotifyRequest{ID: directory, OutputLength: 4096, Filter: 1})
	if status := client.call(t, wire.ChangeNotify, body, 1).Header.Status; status != smb.StatusNotSupported {
		t.Fatalf("CHANGE_NOTIFY status %#x", status)
	}
	client.echo(t)
}
