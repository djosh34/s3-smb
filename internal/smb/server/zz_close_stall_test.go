package server

import (
	"context"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

// Throwaway research test for #620, in the shape of the s3-outage Mac run. A
// FLUSH of a band waits on S3 and holds the band's I/O lock. macOS then
// closes another handle on that band and writes the next band. The WRITE
// must get a reply, interim or final, at once: macOS fails a request with no
// reply after 2 minutes, and the data of a failed write-behind is lost.
func TestZZCloseDuringOutageDoesNotStallConnection(t *testing.T) {
	t.Run("alone", func(t *testing.T) { closeDuringOutage(t, false) })
	t.Run("second in a compound", func(t *testing.T) { closeDuringOutage(t, true) })
}

func closeDuringOutage(t *testing.T, compound bool) {
	srv, proxy := newS3Server(t)
	client := srv.connect(t)
	band := client.open(t, "band")
	other := client.open(t, "band")
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
	answered := false
	for !answered {
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
