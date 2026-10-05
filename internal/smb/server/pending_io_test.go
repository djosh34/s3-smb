package server

import (
	"bytes"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/s3fault"
	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/smbtest"
	"github.com/djosh34/s3-smb/internal/smb/wire"
	"github.com/djosh34/s3-smb/internal/storage"
)

// newS3Server serves storage whose objects sit behind an S3 fault proxy.
func newS3Server(t *testing.T, config smbtest.S3Config) (*testServer, *s3fault.Proxy) {
	t.Helper()
	adapter, proxy := smbtest.NewS3Storage(t, config)
	srv := newTestServerOn(t, adapter)
	t.Cleanup(proxy.Release)
	t.Cleanup(proxy.RestoreS3)
	return srv, proxy
}

// flushOK flushes id and fails the test unless that succeeds.
func flushOK(t *testing.T, client *testClient, id wire.FileID) {
	t.Helper()
	if status := client.flush(t, wire.FlushRequest{ID: id}); status != smb.StatusSuccess {
		t.Fatalf("FLUSH status %#x", status)
	}
}

// ioCommands are the requests that wait on S3.
var ioCommands = []struct {
	name    string
	command wire.Command
}{{"READ", wire.Read}, {"WRITE", wire.Write}, {"FLUSH", wire.Flush}}

// sendIO sends a READ of 64 bytes at offset, a write-through WRITE of data
// at offset, or a FLUSH of id.
func sendIO(t *testing.T, client *testClient, command wire.Command, id wire.FileID, offset uint64, data []byte) wire.Header {
	t.Helper()
	if command == wire.Read {
		return client.send(t, wire.Read, encode(t, wire.EncodeReadRequest, wire.ReadRequest{ID: id, Offset: offset, Length: 64}), 1)
	}
	if command == wire.Write {
		return client.send(t, wire.Write, encode(t, wire.EncodeWriteRequest, wire.WriteRequest{ID: id, Offset: offset, Data: data, Flags: writeThrough}), creditsFor(len(data)))
	}
	return client.send(t, wire.Flush, encode(t, wire.EncodeFlushRequest, wire.FlushRequest{ID: id}), 1)
}

func readOK(t *testing.T, client *testClient, request wire.ReadRequest, want []byte) {
	t.Helper()
	if data, status := client.read(t, request); status != smb.StatusSuccess || !bytes.Equal(data, want) {
		t.Fatalf("READ at %d = %d bytes, %#x; want %d bytes", request.Offset, len(data), status, len(want))
	}
}

// S3 holds the upload's response, so the request stays in storage until the
// test releases it. Meanwhile the connection keeps serving other requests.
func TestSlowS3UploadRepliesAsync(t *testing.T) {
	for _, test := range ioCommands[1:] {
		t.Run(test.name, func(t *testing.T) {
			srv, proxy := newS3Server(t, smbtest.S3Config{Timeout: time.Minute})
			client := srv.connect(t)
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
			if test.command == wire.Flush {
				data = data[:32]
				writeFile(t, client, slow, data)
			}
			request := sendIO(t, client, test.command, slow, 0, data)
			<-held
			// The interim reply grants the request's credits; the final one grants none.
			if interim := client.interim(t, request); interim.Credit != request.Credit {
				t.Fatalf("interim reply granted %d credits, want %d", interim.Credit, request.Credit)
			}
			client.echo(t)
			readOK(t, client, wire.ReadRequest{ID: other, Length: 1024}, otherData)
			proxy.Release()
			if status := client.receive(t, request).Header.Status; status != smb.StatusSuccess {
				t.Fatalf("final status %#x", status)
			}
			readOK(t, client, wire.ReadRequest{ID: slow, Length: 1 << 17}, data)
		})
	}
}

// During an S3 outage READ, WRITE and FLUSH wait with an interim reply while
// the connection serves cached data, and finish once S3 is back. They share
// one outage: the full outage a backup must survive in gate mode, with the
// daemon's retries.
func TestS3OutageRepliesAsync(t *testing.T) {
	outage := 3 * time.Second
	if os.Getenv("S3_SMB_CHECK_MODE") == "gate" {
		outage = smb.S3OutageWindow
	}
	srv, proxy := newS3Server(t, smbtest.S3Config{MetaRetries: storage.FilesystemRetries, ChunkRetries: storage.UploadRetries})
	client := srv.connect(t)
	cached := client.open(t, "cached")
	cachedData := []byte("cached during the outage")
	writeFile(t, client, cached, cachedData)
	flushOK(t, client, cached)
	readOK(t, client, wire.ReadRequest{ID: cached, Length: 64}, cachedData)
	// READ crosses the first 64 KiB block boundary from a cold cache.
	stored := bytes.Repeat([]byte("block boundary payload\n"), 6000)
	offset := uint64(64<<10 - 32)
	read := client.open(t, "read")
	writeFile(t, client, read, stored)
	flushOK(t, client, read)
	written, flushed := []byte("written during the outage"), []byte("flushed during the outage")
	write, flush := client.open(t, "write"), client.open(t, "flush")
	writeFile(t, client, flush, flushed)

	start := proxy.FailS3For(outage)
	requests := []wire.Header{
		sendIO(t, client, wire.Read, read, offset, nil),
		sendIO(t, client, wire.Write, write, 0, written),
		sendIO(t, client, wire.Flush, flush, 0, nil),
	}
	for _, request := range requests {
		client.interim(t, request)
	}
	<-proxy.OutageSeen()
	client.echo(t)
	readOK(t, client, wire.ReadRequest{ID: cached, Length: 64}, cachedData)
	finals := make([]wire.Message, len(requests))
	for i, request := range requests {
		if finals[i] = client.receive(t, request); finals[i].Header.Status != smb.StatusSuccess {
			t.Fatalf("%v final status %#x", request.Command, finals[i].Header.Status)
		}
	}
	if time.Since(start) < outage {
		t.Fatal("finished before S3 returned")
	}
	if response, err := wire.DecodeReadResponse(finals[0]); err != nil || !bytes.Equal(response.Data, stored[offset:offset+64]) {
		t.Fatalf("READ = %q, %v", response.Data, err)
	}
	srv.expectContent(t, map[string]string{"write": string(written), "flush": string(flushed)})
}

// When S3 keeps failing past the retry budget, the final reply reports the
// error under the interim reply's async ID and the connection goes on.
func TestS3FailureEndsAsyncRequest(t *testing.T) {
	for _, test := range ioCommands {
		t.Run(test.name, func(t *testing.T) {
			srv, proxy := newS3Server(t, smbtest.S3Config{ReadRetryWindow: 100 * time.Millisecond})
			client := srv.connect(t)
			id := client.open(t, "file")
			data := []byte("failed upload")
			method := http.MethodPut
			if test.command != wire.Write {
				writeFile(t, client, id, data)
			}
			if test.command == wire.Read {
				flushOK(t, client, id)
				method = http.MethodGet
			}
			if err := proxy.SetFault(s3fault.Fault{Method: method, Status: http.StatusServiceUnavailable}); err != nil {
				t.Fatal(err)
			}
			request := sendIO(t, client, test.command, id, 0, data)
			client.interim(t, request)
			if status := client.receive(t, request).Header.Status; status != smb.StatusIODeviceError {
				t.Fatalf("final status %#x", status)
			}
			client.echo(t)
			// Data that failed to upload stays failed, so CLOSE reports it too.
			want := smb.StatusIODeviceError
			if test.command == wire.Read {
				want = smb.StatusSuccess
			}
			if status := client.close(t, id); status != want {
				t.Fatalf("CLOSE status %#x, want %#x", status, want)
			}
		})
	}
}

// A READ waiting on S3 inside a compound holds back only the request related
// to it. The request before it and an unrelated one after it answer at once,
// and nothing is answered twice.
func TestS3OutageInCompound(t *testing.T) {
	srv, proxy := newS3Server(t, smbtest.S3Config{ChunkRetries: 5})
	client := srv.connect(t)
	id := client.open(t, "file")
	data := []byte("cold compound data")
	writeFile(t, client, id, data)
	flushOK(t, client, id)
	proxy.FailS3For(time.Hour)

	echo := encode(t, wire.EncodeEchoRequest, wire.EmptyRequest{})
	prefix := wire.Message{Header: client.header(wire.Echo, 1), Body: echo}
	read := wire.Message{Header: client.header(wire.Read, 1), Body: encode(t, wire.EncodeReadRequest, wire.ReadRequest{ID: id, Length: 64})}
	related := wire.Message{Header: client.header(wire.Echo, 1), Body: echo}
	related.Header.Flags |= wire.FlagRelated
	related.Header.SessionID, related.Header.TreeID = ^uint64(0), ^uint32(0)
	unrelated := wire.Message{Header: client.header(wire.Echo, 1), Body: echo}
	if err := client.raw.Send(t.Context(), []wire.Message{prefix, read, related, unrelated}); err != nil {
		t.Fatal(err)
	}
	if status := client.receive(t, prefix.Header).Header.Status; status != smb.StatusSuccess {
		t.Fatalf("prefix status %#x", status)
	}
	readInterim, relatedInterim := client.interim(t, read.Header), client.interim(t, related.Header)
	if readInterim.AsyncID == relatedInterim.AsyncID {
		t.Fatal("related request shares the READ's async ID")
	}
	if status := client.receive(t, unrelated.Header).Header.Status; status != smb.StatusSuccess {
		t.Fatalf("unrelated status %#x", status)
	}
	<-proxy.OutageSeen()
	proxy.RestoreS3()
	if response, status := decodeReply(t, client.receive(t, read.Header), wire.DecodeReadResponse); status != smb.StatusSuccess || !bytes.Equal(response.Data, data) {
		t.Fatalf("READ = %q, %#x", response.Data, status)
	}
	if status := client.receive(t, related.Header).Header.Status; status != smb.StatusSuccess {
		t.Fatalf("related status %#x", status)
	}
	client.noExtraReplies(t)
}
