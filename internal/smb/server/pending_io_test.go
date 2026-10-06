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
)

// gateOutage is the S3 outage a backup must survive in gate mode.
const gateOutage = 5 * time.Minute

// newS3Server serves an engine whose bucket sits behind an S3 fault proxy.
func newS3Server(t *testing.T) (*testServer, *s3fault.Proxy) {
	t.Helper()
	storage, proxy := smbtest.NewS3Storage(t)
	srv := newTestServerOn(t, storage)
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
// The engine uploads at FLUSH, so the WRITE is a write-through one.
func TestSlowS3UploadRepliesAsync(t *testing.T) {
	for _, test := range ioCommands[1:] {
		t.Run(test.name, func(t *testing.T) {
			srv, proxy := newS3Server(t)
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
// the connection serves data still in RAM, and finish once S3 is back. They
// share one outage: the full outage a backup must survive in gate mode, with
// the engine's own retries.
func TestS3OutageRepliesAsync(t *testing.T) {
	outage := 3 * time.Second
	if os.Getenv("S3_SMB_CHECK_MODE") == "gate" {
		outage = gateOutage
	}
	srv, proxy := newS3Server(t)
	client := srv.connect(t)
	// Written but not flushed, so it stays in RAM and needs no S3.
	cached := client.open(t, "cached")
	cachedData := []byte("cached during the outage")
	writeFile(t, client, cached, cachedData)
	readOK(t, client, wire.ReadRequest{ID: cached, Length: 64}, cachedData)
	// READ fetches flushed data from S3.
	stored := bytes.Repeat([]byte("stored payload\n"), 6000)
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
	// Every final reply must come after S3 is back. Replies sent before the
	// cached READ's reply are buffered already; the rest are timed as they
	// arrive.
	finals := make(map[uint64]wire.Message)
	for _, request := range requests {
		if buffered := client.replies[request.MessageID]; len(buffered) != 0 {
			if time.Since(start) < outage {
				t.Fatalf("%v finished during the outage", request.Command)
			}
			finals[request.MessageID] = buffered[0]
		}
	}
	for len(finals) < len(requests) {
		reply, err := client.raw.Receive(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		for _, message := range reply.Messages {
			if time.Since(start) < outage {
				t.Fatalf("%v finished during the outage", message.Header.Command)
			}
			finals[message.Header.MessageID] = message
		}
	}
	for _, request := range requests {
		if status := finals[request.MessageID].Header.Status; status != smb.StatusSuccess {
			t.Fatalf("%v final status %#x", request.Command, status)
		}
	}
	if response, err := wire.DecodeReadResponse(finals[requests[0].MessageID]); err != nil || !bytes.Equal(response.Data, stored[offset:offset+64]) {
		t.Fatalf("READ = %q, %v", response.Data, err)
	}
	srv.expectContent(t, map[string]string{"write": string(written), "flush": string(flushed)})
}

// When S3 refuses a request, the final reply reports the error under the
// interim reply's async ID and the connection goes on. The engine retries
// unavailable S3 for minutes, so the fault is a slow refusal it does not
// retry.
func TestS3FailureEndsAsyncRequest(t *testing.T) {
	for _, test := range ioCommands {
		t.Run(test.name, func(t *testing.T) {
			srv, proxy := newS3Server(t)
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
			refusal := s3fault.Fault{Method: method, Status: http.StatusForbidden, Code: "AccessDenied", HeaderDelay: 100 * time.Millisecond}
			if err := proxy.SetFault(refusal); err != nil {
				t.Fatal(err)
			}
			request := sendIO(t, client, test.command, id, 0, data)
			client.interim(t, request)
			if status := client.receive(t, request).Header.Status; status != smb.StatusIODeviceError {
				t.Fatalf("final status %#x", status)
			}
			client.echo(t)
			// CLOSE does not upload, so it succeeds and leaves the data in RAM.
			if status := client.close(t, id); status != smb.StatusSuccess {
				t.Fatalf("CLOSE status %#x", status)
			}
			// The data still in RAM uploads at shutdown.
			if err := proxy.SetFault(s3fault.Fault{}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// A READ waiting on S3 inside a compound puts the whole compound on hold:
// macOS reads a compound reply only whole, or as an interim reply for its
// first member followed by one chain. So the ECHO before the READ gets the
// interim reply, and all four replies follow in one chain once S3 is back.
func TestS3OutageInCompound(t *testing.T) {
	srv, proxy := newS3Server(t)
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
	interim := client.interim(t, prefix.Header)
	<-proxy.OutageSeen()
	proxy.RestoreS3()
	reply, err := client.raw.Receive(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(reply.Messages) != 4 {
		t.Fatalf("%d replies in the chain, want 4", len(reply.Messages))
	}
	for i, request := range []wire.Message{prefix, read, related, unrelated} {
		got := reply.Messages[i].Header
		if got.MessageID != request.Header.MessageID || got.Status != smb.StatusSuccess {
			t.Fatalf("reply %d: %+v, want success for message %d", i, got, request.Header.MessageID)
		}
	}
	if first := reply.Messages[0].Header; first.Flags&wire.FlagAsync == 0 || first.AsyncID != interim.AsyncID || first.Credit != 0 {
		t.Fatalf("first reply %+v after interim %+v", first, interim)
	}
	if response, err := wire.DecodeReadResponse(reply.Messages[1]); err != nil || !bytes.Equal(response.Data, data) {
		t.Fatalf("READ = %q, %v", response.Data, err)
	}
	client.noExtraReplies(t)
}

// A CLOSE waits for the open's pending WRITE, which waits on S3. It must reply
// STATUS_PENDING and let the connection go on: macOS fails a request with no
// reply after 2 minutes, and data written through it is lost.
func TestCloseAfterSlowWriteRepliesAsync(t *testing.T) {
	srv, proxy := newS3Server(t)
	client := srv.connect(t)
	id := client.open(t, "slow")
	held, err := proxy.HoldNextChunkResponse()
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("written before the close")
	write := sendIO(t, client, wire.Write, id, 0, data)
	<-held
	client.interim(t, write)
	closing := client.send(t, wire.Close, encode(t, wire.EncodeCloseRequest, wire.CloseRequest{ID: id}), 1)
	client.interim(t, closing)
	client.echo(t)
	proxy.Release()
	for _, request := range []wire.Header{write, closing} {
		if status := client.receive(t, request).Header.Status; status != smb.StatusSuccess {
			t.Fatalf("%v final status %#x", request.Command, status)
		}
	}
	srv.expectContent(t, map[string]string{"slow": string(data)})
}
