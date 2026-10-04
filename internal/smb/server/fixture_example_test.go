package server

import (
	"bytes"
	"fmt"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/smbtest"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func TestFixtureReadSeesWriteFromAnotherOpen(t *testing.T) {
	client := newTestServer(t).connect(t)
	writer := client.open(t, "coherent")
	reader := client.open(t, "coherent")
	if status := client.write(t, wire.WriteRequest{ID: writer, Offset: 2, Data: []byte("hello")}); status != smb.StatusSuccess {
		t.Fatalf("WRITE status %#x", status)
	}
	for _, test := range []struct {
		name, data      string
		offset          uint64
		length, minimum uint32
		want            smb.Status
	}{
		{"short read", "hello", 2, 10, 0, smb.StatusSuccess},
		{"minimum met", "hello", 2, 10, 5, smb.StatusSuccess},
		{"minimum unmet", "", 2, 10, 6, smb.StatusEndOfFile},
		{"minimum exceeds request", "", 2, 3, 4, smb.StatusEndOfFile},
		{"at EOF", "", 7, 1, 0, smb.StatusEndOfFile},
		{"past EOF", "", 8, 1, 0, smb.StatusEndOfFile},
		{"zero length", "", 7, 0, 0, smb.StatusSuccess},
		{"zero length minimum", "", 7, 0, 1, smb.StatusEndOfFile},
		{"hole", "\x00\x00", 0, 2, 0, smb.StatusSuccess},
	} {
		t.Run(test.name, func(t *testing.T) {
			data, status := client.read(t, wire.ReadRequest{ID: reader, Offset: test.offset, Length: test.length, MinimumCount: test.minimum})
			if status != test.want || string(data) != test.data {
				t.Fatalf("READ = %q, %#x; want %q, %#x", data, status, test.data, test.want)
			}
		})
	}
}

// S3 holds the upload's response, so the request stays in storage until the
// test releases it. Meanwhile the connection must keep serving other requests.
func TestFixtureSlowS3UploadRepliesAsync(t *testing.T) {
	for _, test := range []struct {
		name    string
		command wire.Command
	}{{"write", wire.Write}, {"flush", wire.Flush}} {
		t.Run(test.name, func(t *testing.T) {
			adapter, proxy := smbtest.NewS3Storage(t, smbtest.S3Config{Timeout: time.Minute})
			client := newTestServerOn(t, adapter).connect(t)
			t.Cleanup(proxy.Release)
			other := client.open(t, "other")
			otherData := []byte("unrelated bytes")
			if status := client.write(t, wire.WriteRequest{ID: other, Data: otherData, Flags: writeThrough}); status != smb.StatusSuccess {
				t.Fatalf("WRITE status %#x", status)
			}
			slow := client.open(t, "slow")
			held, err := proxy.HoldNextChunkResponse()
			if err != nil {
				t.Fatal(err)
			}
			// More than one credit also checks credits for multi-credit async I/O.
			data := bytes.Repeat([]byte("slow storage bytes\n"), 4000)
			var request wire.Header
			if test.command == wire.Write {
				body := encode(t, wire.EncodeWriteRequest, wire.WriteRequest{ID: slow, Data: data, Flags: writeThrough})
				request = client.send(t, wire.Write, body, creditsFor(len(data)))
			} else {
				data = data[:32]
				if status := client.write(t, wire.WriteRequest{ID: slow, Data: data}); status != smb.StatusSuccess {
					t.Fatalf("WRITE status %#x", status)
				}
				request = client.send(t, wire.Flush, encode(t, wire.EncodeFlushRequest, wire.FlushRequest{ID: slow}), 1)
			}
			<-held
			// The interim reply grants the request's credits; the final one grants none.
			if interim := client.interim(t, request); interim.Credit != request.Credit {
				t.Fatalf("interim reply granted %d credits, want %d", interim.Credit, request.Credit)
			}
			client.echo(t)
			if got, status := client.read(t, wire.ReadRequest{ID: other, Length: 1024}); status != smb.StatusSuccess || !bytes.Equal(got, otherData) {
				t.Fatalf("unrelated READ = %q, %#x", got, status)
			}
			proxy.Release()
			if status := client.receive(t, request).Header.Status; status != smb.StatusSuccess {
				t.Fatalf("final status %#x", status)
			}
			if got, status := client.read(t, wire.ReadRequest{ID: slow, Length: 1 << 17}); status != smb.StatusSuccess || !bytes.Equal(got, data) {
				t.Fatalf("READ after upload = %d bytes, %#x", len(got), status)
			}
		})
	}
}

func TestFixtureSetInfoAllocationBelowEOFShrinks(t *testing.T) {
	for _, allocation := range []uint64{0, 3, 7, 8192} {
		t.Run(fmt.Sprint(allocation), func(t *testing.T) {
			client := newTestServer(t).connect(t)
			id := client.open(t, "file")
			if status := client.write(t, wire.WriteRequest{ID: id, Data: []byte("1234567")}); status != smb.StatusSuccess {
				t.Fatalf("WRITE status %#x", status)
			}
			input := encode(t, wire.EncodeFileAllocationInformation, wire.FileAllocationInformation{AllocationSize: allocation})
			if status := client.setInfo(t, wire.SetInfoRequest{ID: id, InfoType: wire.InfoFile, InfoClass: uint8(wire.ClassFileAllocation), Input: input}); status != smb.StatusSuccess {
				t.Fatalf("SET_INFO status %#x", status)
			}
			// Allocation rounds up to whole clusters, so only zero is below EOF.
			want := uint64(7)
			if allocation == 0 {
				want = 0
			}
			endOfFile := func() uint64 {
				data, status := client.queryInfo(t, wire.QueryInfoRequest{ID: id, InfoType: wire.InfoFile, InfoClass: uint8(wire.ClassFileStandard), OutputLength: 1024})
				if status != smb.StatusSuccess {
					t.Fatalf("QUERY_INFO status %#x", status)
				}
				standard, err := wire.DecodeFileStandardInformation(data)
				if err != nil {
					t.Fatal(err)
				}
				return standard.EndOfFile
			}
			if got := endOfFile(); got != want {
				t.Fatalf("EOF = %d, want %d", got, want)
			}
			if status := client.flush(t, wire.FlushRequest{ID: id}); status != smb.StatusSuccess {
				t.Fatalf("FLUSH status %#x", status)
			}
			if got := endOfFile(); got != want {
				t.Fatalf("EOF after flush = %d, want %d", got, want)
			}
		})
	}
}

func TestFixtureDirectoryEmptyRootListsDots(t *testing.T) {
	client := newTestServer(t).connect(t)
	root, status := client.create(t, smbtest.CreateOptions{Request: wire.CreateRequest{DesiredAccess: fileGenericRead, ShareAccess: 7, Disposition: fileOpen, Options: fileDirectoryFile}})
	if status != smb.StatusSuccess {
		t.Fatalf("root CREATE status %#x", status)
	}
	query := wire.QueryDirectoryRequest{ID: root.Reply.ID, Pattern: "*", Flags: directoryReopen, InfoClass: wire.ClassDirectoryNames, OutputLength: 4096}
	data, status := client.queryDirectory(t, query)
	if status != smb.StatusSuccess {
		t.Fatalf("QUERY_DIRECTORY status %#x", status)
	}
	entries, err := wire.DecodeDirectoryNamesEntries(data)
	if err != nil || len(entries) != 2 || entries[0].Name != "." || entries[1].Name != ".." {
		t.Fatalf("empty root lists %+v, %v", entries, err)
	}
	query.Pattern, query.Flags = "", 0
	if _, status = client.queryDirectory(t, query); status != smb.StatusNoMoreFiles {
		t.Fatalf("continuation status %#x", status)
	}
	if status = client.close(t, root.Reply.ID); status != smb.StatusSuccess {
		t.Fatalf("CLOSE status %#x", status)
	}
}

func TestFixtureIOCTLOnOpenFileIsRefused(t *testing.T) {
	client := newTestServer(t).connect(t)
	id := client.open(t, "file")
	// FSCTL_SRV_REQUEST_RESUME_KEY, which server-side copy would need.
	if status := client.ioctl(t, wire.IOCTLRequest{ID: id, ControlCode: 0x00140078, Flags: 1, MaxOutput: 1024}); status != smb.StatusNotSupported {
		t.Fatalf("IOCTL status %#x", status)
	}
	client.echo(t)
}
