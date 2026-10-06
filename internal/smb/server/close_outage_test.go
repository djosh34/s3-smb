package server

import (
	"context"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

// A FLUSH of a band waits on S3 and holds the band's I/O lock. macOS then
// closes another handle on that band, and writes the next band. The WRITE
// must get a reply, interim or final, at once: macOS fails a request with no
// reply after 2 minutes, and the data of a failed write-behind is lost. The
// CLOSE runs alone or second in a compound, of another handle or of the
// handle whose FLUSH waits.
func TestCloseDuringOutageDoesNotStallConnection(t *testing.T) {
	t.Run("alone", func(t *testing.T) { closeDuringOutage(t, false, false) })
	t.Run("second in a compound", func(t *testing.T) { closeDuringOutage(t, true, false) })
	t.Run("same handle second in a compound", func(t *testing.T) { closeDuringOutage(t, true, true) })
}

func closeDuringOutage(t *testing.T, compound, same bool) {
	srv, proxy := newS3Server(t)
	client := srv.connect(t)
	band := client.open(t, "band")
	other := client.open(t, "band")
	if same {
		other = band
	}
	next := client.open(t, "next")
	writeFile(t, client, band, []byte("band data waiting for S3"))
	held, err := proxy.HoldNextChunkResponse()
	if err != nil {
		t.Fatal(err)
	}
	flush := sendIO(t, client, wire.Flush, band, 0, nil)
	<-held
	client.interim(t, flush)
	var closing wire.Header
	if compound {
		echo := wire.Message{Header: client.header(wire.Echo, 1), Body: encode(t, wire.EncodeEchoRequest, wire.EmptyRequest{})}
		closeMessage := wire.Message{Header: client.header(wire.Close, 1), Body: encode(t, wire.EncodeCloseRequest, wire.CloseRequest{ID: other})}
		if err := client.raw.Send(t.Context(), []wire.Message{echo, closeMessage}); err != nil {
			t.Fatal(err)
		}
		closing = closeMessage.Header
		defer func() { client.receive(t, echo.Header) }()
	} else {
		closing = client.send(t, wire.Close, encode(t, wire.EncodeCloseRequest, wire.CloseRequest{ID: other}), 1)
	}
	data := []byte("next band, written while S3 is down")
	write := client.send(t, wire.Write, encode(t, wire.EncodeWriteRequest, wire.WriteRequest{ID: next, Data: data}), 1)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	for answered := false; !answered; {
		reply, err := client.raw.Receive(ctx)
		if err != nil {
			proxy.Release()
			t.Fatalf("no reply to the WRITE within 3 seconds; the connection is stalled behind the CLOSE: %v", err)
		}
		for _, message := range reply.Messages {
			client.replies[message.Header.MessageID] = append(client.replies[message.Header.MessageID], message)
			answered = answered || message.Header.MessageID == write.MessageID
		}
	}
	proxy.Release()
	for _, request := range []wire.Header{flush, closing, write} {
		if status := client.receive(t, request).Header.Status; status != smb.StatusSuccess {
			t.Fatalf("%v final status %#x", request.Command, status)
		}
	}
}

// LOGOFF, TREE_DISCONNECT and a reconnect that replaces the session stop the
// session's requests and wait for them. A READ that waits for the band's I/O
// lock, which a FLUSH on another connection holds while S3 is down, cannot
// stop. The cleanup request must still get a reply, interim or final, at once.
func TestCleanupDuringOutageRepliesAtOnce(t *testing.T) {
	for _, test := range []struct {
		cleanup func(t *testing.T, client *testClient, release func())
		name    string
	}{
		{name: "LOGOFF", cleanup: func(t *testing.T, client *testClient, release func()) {
			leaveAsync(t, client, client.send(t, wire.Logoff, encode(t, wire.EncodeLogoffRequest, wire.EmptyRequest{}), 1), release)
		}},
		{name: "TREE_DISCONNECT", cleanup: func(t *testing.T, client *testClient, release func()) {
			leaveAsync(t, client, client.send(t, wire.TreeDisconnect, encode(t, wire.EncodeTreeDisconnectRequest, wire.EmptyRequest{}), 1), release)
		}},
		{name: "reconnect", cleanup: func(t *testing.T, client *testClient, release func()) {
			client.reconnect(t)
			release()
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			srv, proxy := newS3Server(t)
			flusher := srv.connect(t)
			band := flusher.open(t, "band")
			writeFile(t, flusher, band, []byte("band data waiting for S3"))
			leaver := srv.connect(t)
			other := leaver.open(t, "band")
			held, err := proxy.HoldNextChunkResponse()
			if err != nil {
				t.Fatal(err)
			}
			flush := sendIO(t, flusher, wire.Flush, band, 0, nil)
			<-held
			flusher.interim(t, flush)
			leaver.interim(t, sendIO(t, leaver, wire.Read, other, 0, nil))
			stalled := time.AfterFunc(3*time.Second, proxy.Release)
			test.cleanup(t, leaver, func() {
				if !stalled.Stop() {
					t.Error("no reply within 3 seconds: the cleanup waited in the request loop")
				}
				proxy.Release()
			})
			if status := flusher.receive(t, flush).Header.Status; status != smb.StatusSuccess {
				t.Fatalf("FLUSH status %#x", status)
			}
		})
	}
}

// leaveAsync expects an interim reply to request, then calls release and
// expects the final reply to succeed.
func leaveAsync(t *testing.T, client *testClient, request wire.Header, release func()) {
	t.Helper()
	reply := client.next(t, request)
	release()
	if reply.Header.Status != smb.StatusPending {
		t.Fatalf("%v answered %#x before its session's requests ended", request.Command, reply.Header.Status)
	}
	client.pending(t, reply.Header)
	if status := client.receive(t, request).Header.Status; status != smb.StatusSuccess {
		t.Fatalf("%v status %#x", request.Command, status)
	}
}

// macOS fails a request 2 minutes after its last interim reply. A FLUSH that
// waits on S3 longer gets its interim reply again, with the same async ID.
func TestLongWaitRepeatsInterim(t *testing.T) {
	interimRepeat = 50 * time.Millisecond
	t.Cleanup(func() { interimRepeat = 30 * time.Second })
	srv, proxy := newS3Server(t)
	client := srv.connect(t)
	band := client.open(t, "band")
	writeFile(t, client, band, []byte("band data waiting for S3"))
	held, err := proxy.HoldNextChunkResponse()
	if err != nil {
		t.Fatal(err)
	}
	flush := sendIO(t, client, wire.Flush, band, 0, nil)
	<-held
	first := client.interim(t, flush)
	again := client.next(t, flush).Header
	proxy.Release()
	if again.Status != smb.StatusPending || again.AsyncID != first.AsyncID || again.Credit != 0 {
		t.Fatalf("second reply %+v, want the interim reply %+v again without credits", again, first)
	}
	for {
		reply := client.next(t, flush).Header
		if reply.Status == smb.StatusPending {
			continue
		}
		if reply.Status != smb.StatusSuccess || reply.AsyncID != first.AsyncID {
			t.Fatalf("final reply %+v", reply)
		}
		break
	}
}
