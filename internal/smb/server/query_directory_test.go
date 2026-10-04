package server

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/state"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func TestDirectoryContinuationReusesOriginalPattern(t *testing.T) {
	f := newDirectoryFixture(t)
	f.create(t, "alpha", smb.KindFile)
	f.create(t, "beta", smb.KindFile)
	open := f.open(t, "", 1)
	for i, want := range []string{".", "..", "alpha", "beta"} {
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
	want := map[string]bool{".": true, "..": true}
	for i := range 150 {
		name := fmt.Sprintf("entry-%03d", i)
		f.create(t, name, smb.KindFile)
		want[name] = true
	}
	open := f.open(t, "", 1)
	seen := make(map[string]bool)
	for page := range 153 {
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
	}
	for _, test := range cases {
		status, entries := f.query(t, open, test.pattern, test.flags, wire.ClassDirectoryNames, 4096)
		if status != smb.StatusSuccess || !reflect.DeepEqual(directoryNames(entries), []string{test.want}) {
			t.Fatalf("%+v: %#x, %v", test, status, directoryNames(entries))
		}
	}
	status, _ := f.query(t, open, "", 0, wire.ClassDirectoryNames, 4096)
	if status != smb.StatusNoMoreFiles {
		t.Fatalf("exhausted search: %#x", status)
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

func TestDirectoryFilteredDots(t *testing.T) {
	f := newDirectoryFixture(t)
	f.create(t, "child", smb.KindDirectory)
	f.create(t, "child/alpha.txt", smb.KindFile)
	open := f.open(t, "child", 1)
	for _, test := range []struct {
		pattern string
		want    []string
	}{
		{"*", []string{".", "..", "alpha.txt"}},
		{"*.*", []string{".", "..", "alpha.txt"}},
		{".*", []string{".", ".."}},
		{".", []string{"."}},
		{"..", []string{".."}},
		{"alpha*", []string{"alpha.txt"}},
	} {
		t.Run(test.pattern, func(t *testing.T) {
			for i, name := range test.want {
				pattern, flags := "", uint8(directorySingle)
				if i == 0 {
					pattern, flags = test.pattern, directoryReopen|directorySingle
				}
				status, entries := f.query(t, open, pattern, flags, wire.ClassDirectoryNames, 4096)
				if status != smb.StatusSuccess || !reflect.DeepEqual(directoryNames(entries), []string{name}) {
					t.Fatalf("entry %d: %#x, %v, want %q", i, status, directoryNames(entries), name)
				}
			}
			status, _ := f.query(t, open, "", directorySingle, wire.ClassDirectoryNames, 4096)
			if status != smb.StatusNoMoreFiles {
				t.Fatalf("exhausted search: %#x", status)
			}
		})
	}
}

func TestDirectoryEmptyRootListsDots(t *testing.T) {
	f := newDirectoryFixture(t)
	request := createRequest("", fileOpen)
	request.Options = fileDirectoryFile
	created := createdFile(t, fileCreate(f.ctx, t, f.client, f.session, f.next, request))
	f.next++
	root := state.Open{ID: state.FileID(created.ID)}
	attr, err := f.server.options.Storage.Lookup(f.ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, class := range []wire.DirectoryInfoClass{wire.ClassDirectory, wire.ClassDirectoryFull, wire.ClassDirectoryBoth, wire.ClassDirectoryNames, wire.ClassDirectoryIDBoth, wire.ClassDirectoryIDFull} {
		status, entries := f.query(t, root, "*", directoryReopen, class, 4096)
		if status != smb.StatusSuccess || !reflect.DeepEqual(directoryNames(entries), []string{".", ".."}) {
			t.Fatalf("class %d empty root: %#x, %v", class, status, directoryNames(entries))
		}
		for _, entry := range entries {
			if class != wire.ClassDirectoryNames && entry.Metadata.Basic.Attributes&0x10 == 0 {
				t.Fatalf("root dot is not a directory: %+v", entry)
			}
			if (class == wire.ClassDirectoryIDBoth || class == wire.ClassDirectoryIDFull) && entry.Metadata.FileID != uint64(attr.Object.Inode) {
				t.Fatalf("root dot escapes the share: %+v", entry)
			}
		}
		status, _ = f.query(t, root, "", 0, class, 4096)
		if status != smb.StatusNoMoreFiles {
			t.Fatalf("class %d empty root continuation: %#x", class, status)
		}
	}
	status, _ := f.query(t, root, "*", directoryReopen, wire.ClassDirectoryNames, 1)
	if status != smb.StatusBufferTooSmall {
		t.Fatalf("small root buffer: %#x", status)
	}
	for _, name := range []string{".", ".."} {
		stepStatus, entries := f.query(t, root, "", directorySingle, wire.ClassDirectoryNames, 16)
		if stepStatus != smb.StatusSuccess || !reflect.DeepEqual(directoryNames(entries), []string{name}) {
			t.Fatalf("root continuation: %#x, %v, want %q", stepStatus, directoryNames(entries), name)
		}
	}
	status, _ = f.query(t, root, "missing", directoryReopen, wire.ClassDirectoryNames, 4096)
	if status != smb.StatusNoSuchFile {
		t.Fatalf("unmatched root filter: %#x", status)
	}
	status, _ = f.query(t, root, "", 0, wire.ClassDirectoryNames, 4096)
	if status != smb.StatusNoMoreFiles {
		t.Fatalf("unmatched root continuation: %#x", status)
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
		{&root, "*", 4096, smb.StatusNotSupported, 0x3c, 0},
		{&root, "*", 4096, smb.StatusNotSupported, 0x4e, 0},
		{&root, "*", 4096, smb.StatusNotSupported, 0x4f, 0},
		{&root, "*", 4096, smb.StatusNotSupported, 0x50, 0},
		{&root, "*", 4096, smb.StatusNotSupported, 0x51, 0},
		{&root, "*", 4096, smb.StatusInvalidInfoClass, 0x3b, 0},
		{&root, "*", 4096, smb.StatusInvalidInfoClass, 0x3d, 0},
		{&root, "*", 4096, smb.StatusInvalidInfoClass, 0x4d, 0},
		{&root, "*", 4096, smb.StatusInvalidInfoClass, 0x52, 0},
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
	for i, want := range []string{".", "..", "alpha", "beta"} {
		status, entries := f.query(t, open, "*", directoryIndex|directorySingle, wire.ClassDirectoryNames, 4096)
		if status != smb.StatusSuccess || !reflect.DeepEqual(directoryNames(entries), []string{want}) {
			t.Fatalf("query %d index changed position: %#x, %+v", i, status, entries)
		}
	}
}
