package server

import (
	"bytes"
	"context"
	"fmt"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/smbtest"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

// These tests exercise named objects through protected raw SMB messages, not
// adapter calls. The storage fixture uses SQLite and file-backed JuiceFS data.
type streamClient struct {
	server  *Server
	client  *smbtest.Client
	ctx     context.Context
	session smbtest.Session
	next    uint64
}

func newStreamClient(t *testing.T) *streamClient {
	t.Helper()
	options := testOptions(t)
	options.Storage = newFilesMetaStorage(t)
	server, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	client, ctx, session := loginClient(t, server, smb.CipherAES128GCM, smb.SigningCMAC)
	return &streamClient{server: server, client: client, ctx: ctx, session: session, next: session.NextMessageID}
}

func (c *streamClient) call(t *testing.T, command wire.Command, body []byte) wire.Message {
	t.Helper()
	message := wire.Message{Header: wire.Header{Command: command, MessageID: c.next, SessionID: c.session.SessionID, TreeID: c.session.TreeID, CreditCharge: 1, Credit: 1}, Body: body}
	c.next++
	if err := c.client.Send(c.ctx, []wire.Message{message}); err != nil {
		t.Fatal(err)
	}
	for {
		reply, err := c.client.Receive(c.ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(reply.Messages) != 1 || reply.Messages[0].Header.MessageID != message.Header.MessageID {
			t.Fatalf("unexpected reply: %+v", reply.Messages)
		}
		if reply.Messages[0].Header.Status != smb.StatusPending {
			return reply.Messages[0]
		}
	}
}

func streamRequest(name string, disposition uint32) wire.CreateRequest {
	return wire.CreateRequest{Name: name, Disposition: disposition, DesiredAccess: 0x10000000, ShareAccess: 7} // GENERIC_ALL.
}

func (c *streamClient) create(t *testing.T, request wire.CreateRequest, status smb.Status) wire.CreateResponse {
	t.Helper()
	body, err := wire.EncodeCreateRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	reply := c.call(t, wire.Create, body)
	if reply.Header.Status != status {
		t.Fatalf("CREATE %q disposition %d: status %x, want %x", request.Name, request.Disposition, reply.Header.Status, status)
	}
	if status != smb.StatusSuccess {
		return wire.CreateResponse{}
	}
	result, err := wire.DecodeCreateResponse(reply)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func (c *streamClient) close(t *testing.T, id wire.FileID) wire.CloseResponse {
	t.Helper()
	body, err := wire.EncodeCloseRequest(wire.CloseRequest{ID: id, Flags: 1})
	if err != nil {
		t.Fatal(err)
	}
	reply := c.call(t, wire.Close, body)
	if reply.Header.Status != smb.StatusSuccess {
		t.Fatalf("CLOSE: %x", reply.Header.Status)
	}
	result, err := wire.DecodeCloseResponse(reply)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func (c *streamClient) write(t *testing.T, id wire.FileID, data []byte, offset uint64, status smb.Status) {
	t.Helper()
	body, err := wire.EncodeWriteRequest(wire.WriteRequest{ID: id, Data: data, Offset: offset})
	if err != nil {
		t.Fatal(err)
	}
	reply := c.call(t, wire.Write, body)
	if reply.Header.Status != status {
		t.Fatalf("WRITE at %d: %x, want %x", offset, reply.Header.Status, status)
	}
	if status != smb.StatusSuccess {
		return
	}
	result, err := wire.DecodeWriteResponse(reply)
	if err != nil {
		t.Fatal(err)
	}
	if uint64(result.Count) != uint64(len(data)) {
		t.Fatalf("WRITE count: %d, want %d", result.Count, len(data))
	}
}

func (c *streamClient) read(t *testing.T, id wire.FileID, offset uint64, want []byte) {
	t.Helper()
	length := uint64(len(want))
	if length > 65536 {
		t.Fatal("stream test read exceeds one credit")
		return
	}
	body, err := wire.EncodeReadRequest(wire.ReadRequest{ID: id, Offset: offset, Length: uint32(length)})
	if err != nil {
		t.Fatal(err)
	}
	reply := c.call(t, wire.Read, body)
	if reply.Header.Status != smb.StatusSuccess {
		t.Fatalf("READ at %d: %x", offset, reply.Header.Status)
	}
	result, err := wire.DecodeReadResponse(reply)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(result.Data, want) {
		t.Fatalf("READ at %d: got %q, want %q", offset, result.Data, want)
	}
}

func (c *streamClient) resize(t *testing.T, id wire.FileID, size uint64, status smb.Status) {
	t.Helper()
	input, err := wire.EncodeFileEndOfFileInformation(wire.FileEndOfFileInformation{EndOfFile: size})
	if err != nil {
		t.Fatal(err)
	}
	body, err := wire.EncodeSetInfoRequest(wire.SetInfoRequest{ID: id, InfoType: wire.InfoFile, InfoClass: uint8(wire.ClassFileEndOfFile), Input: input})
	if err != nil {
		t.Fatal(err)
	}
	reply := c.call(t, wire.SetInfo, body)
	if reply.Header.Status != status {
		t.Fatalf("SET_INFO EOF %d: %x, want %x", size, reply.Header.Status, status)
	}
}

func (c *streamClient) query(t *testing.T, id wire.FileID, class wire.FileInfoClass) []byte {
	t.Helper()
	body, err := wire.EncodeQueryInfoRequest(wire.QueryInfoRequest{ID: id, InfoType: wire.InfoFile, InfoClass: uint8(class), OutputLength: 4096})
	if err != nil {
		t.Fatal(err)
	}
	reply := c.call(t, wire.QueryInfo, body)
	if reply.Header.Status != smb.StatusSuccess {
		t.Fatalf("QUERY_INFO class %d: %x", class, reply.Header.Status)
	}
	result, err := wire.DecodeQueryInfoResponse(reply)
	if err != nil {
		t.Fatal(err)
	}
	return result.Data
}

func TestIssue94StreamDispositions(t *testing.T) {
	c := newStreamClient(t)
	for _, stream := range []string{"AFP_Resource", "AFP_AfpInfo", "com.apple.FinderInfo", "other.xattr"} {
		for disposition := uint32(0); disposition <= 5; disposition++ {
			for _, presence := range []string{"no-base", "base-only", "stream"} {
				t.Run(fmt.Sprintf("%s/%d/%s", stream, disposition, presence), func(t *testing.T) {
					testStreamDisposition(t, c, stream, disposition, presence)
				})
			}
		}
	}
}

func testStreamDisposition(t *testing.T, c *streamClient, stream string, disposition uint32, presence string) {
	t.Helper()
	base := fmt.Sprintf("data-%s-%d-%s", stream, disposition, presence)
	baseData := []byte("base bytes must survive every stream disposition")
	var baseID wire.FileID
	if presence != "no-base" {
		baseID = c.create(t, streamRequest(base, fileCreateDisposition), smb.StatusSuccess).ID
		c.write(t, baseID, baseData, 0, smb.StatusSuccess)
	}
	name := base + ":" + stream + ":$DATA"
	if presence == "stream" {
		id := c.create(t, streamRequest(name, fileCreateDisposition), smb.StatusSuccess).ID
		c.write(t, id, []byte("old stream"), 0, smb.StatusSuccess)
		c.close(t, id)
	}
	status, action := streamDispositionResult(disposition, presence)
	opened := c.create(t, streamRequest(name, disposition), status)
	if status == smb.StatusSuccess {
		if opened.Action != action {
			t.Fatalf("action: %d, want %d", opened.Action, action)
		}
		wantSize := uint64(0)
		if presence == "stream" && (disposition == fileOpen || disposition == fileOpenIf) {
			wantSize = 10
			c.read(t, opened.ID, 0, []byte("old stream"))
		}
		if opened.Size != wantSize {
			t.Fatalf("CREATE EOF: %d, want %d", opened.Size, wantSize)
		}
		if closed := c.close(t, opened.ID); closed.Size != wantSize {
			t.Fatalf("CLOSE EOF: %d, want %d", closed.Size, wantSize)
		}
	}
	if status == smb.StatusObjectNameCollision {
		id := c.create(t, streamRequest(name, fileOpen), smb.StatusSuccess).ID
		c.read(t, id, 0, []byte("old stream"))
		c.close(t, id)
	}
	if presence == "no-base" {
		c.create(t, streamRequest(base, fileOpen), smb.StatusObjectNameNotFound)
	} else {
		c.read(t, baseID, 0, baseData)
		c.close(t, baseID)
	}
}

func streamDispositionResult(disposition uint32, presence string) (smb.Status, uint32) {
	if presence == "no-base" {
		return smb.StatusObjectNameNotFound, 0
	}
	if presence == "base-only" {
		if disposition == 1 || disposition == 4 {
			return smb.StatusObjectNameNotFound, 0
		}
		return smb.StatusSuccess, 2 // FILE_CREATED.
	}
	switch disposition {
	case 0:
		return smb.StatusSuccess, 0 // FILE_SUPERSEDED.
	case 1, 3:
		return smb.StatusSuccess, 1 // FILE_OPENED.
	case 2:
		return smb.StatusObjectNameCollision, 0
	default:
		return smb.StatusSuccess, 3 // FILE_OVERWRITTEN.
	}
}

func TestStreamOffsetsResizeAndLimit(t *testing.T) {
	c := newStreamClient(t)
	baseData := []byte("ordinary file content stays unchanged")
	base := c.create(t, streamRequest("forked", 2), smb.StatusSuccess).ID
	c.write(t, base, baseData, 0, smb.StatusSuccess)
	for _, stream := range []string{"AFP_Resource", "AFP_AfpInfo", "com.apple.FinderInfo", "other.xattr"} {
		t.Run(stream, func(t *testing.T) {
			id := c.create(t, streamRequest("forked:"+stream, 2), smb.StatusSuccess).ID
			c.write(t, id, []byte("abcdef"), 0, smb.StatusSuccess)
			c.write(t, id, []byte("XY"), 2, smb.StatusSuccess)
			c.read(t, id, 0, []byte("abXYef"))
			c.read(t, id, 2, []byte("XY"))
			c.resize(t, id, 3, smb.StatusSuccess)
			c.resize(t, id, 6, smb.StatusSuccess)
			c.read(t, id, 0, []byte{'a', 'b', 'X', 0, 0, 0})
			c.write(t, id, []byte("Z"), 8, smb.StatusSuccess)
			c.read(t, id, 3, []byte{0, 0, 0, 0, 0, 'Z'})
			c.write(t, id, []byte("!"), smb.MaxStreamSize-1, smb.StatusSuccess)
			c.write(t, id, []byte("!"), smb.MaxStreamSize, smb.StatusFileTooLarge)
			c.write(t, id, []byte("??"), smb.MaxStreamSize-1, smb.StatusFileTooLarge)
			c.resize(t, id, smb.MaxStreamSize+1, smb.StatusFileTooLarge)
			c.read(t, id, smb.MaxStreamSize-2, []byte{0, '!'})
			c.resize(t, id, smb.MaxStreamSize, smb.StatusSuccess)
			if closed := c.close(t, id); closed.Size != smb.MaxStreamSize {
				t.Fatalf("CLOSE EOF: %d", closed.Size)
			}
			c.read(t, base, 0, baseData)
		})
	}
	c.close(t, base)
}
