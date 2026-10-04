package server

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/smbtest"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

// Delay only the metadata mutation. All file operations use the real adapter.
type blockedSetInfoStorage struct {
	smb.Storage
	started chan struct{}
	resume  chan struct{}
	failure error
	calls   atomic.Int32
	once    sync.Once
}

func (s *blockedSetInfoStorage) SetAttr(ctx context.Context, object smb.ObjectKey, change smb.AttrChange) error {
	s.calls.Add(1)
	select {
	case s.started <- struct{}{}:
	default:
	}
	select {
	case <-s.resume:
	case <-ctx.Done():
		return ctx.Err()
	}
	if s.failure != nil {
		return s.failure
	}
	return s.Storage.SetAttr(ctx, object, change)
}

func (s *blockedSetInfoStorage) unblock() {
	s.once.Do(func() { close(s.resume) })
}

func newBlockedSetInfoFixture(t *testing.T, failure error) (*setInfoFixture, *blockedSetInfoStorage) {
	t.Helper()
	storage := &blockedSetInfoStorage{
		Storage: newFilesMetaStorage(t), started: make(chan struct{}, 1), resume: make(chan struct{}), failure: failure,
	}
	options := testOptions(t)
	options.Storage = storage
	server, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	client, ctx, session := newFilesMetaClient(t, server)
	open := insertFilesMetaOpen(t, server, session, "data", 0x102)
	// Unblock before server cleanup, including when an assertion fails.
	t.Cleanup(storage.unblock)
	return &setInfoFixture{storage: storage, client: client, ctx: ctx, session: session, open: open, nextID: session.NextMessageID}, storage
}

func setInfoAsyncRequest(t *testing.T, f *setInfoFixture, class wire.FileInfoClass) wire.Message {
	t.Helper()
	var input []byte
	var err error
	switch uint8(class) {
	case uint8(wire.ClassFileBasic):
		value := filetime(t, time.Date(2001, 1, 2, 3, 4, 5, 0, time.UTC))
		input, err = wire.EncodeFileBasicInformation(wire.FileBasicInformation{Accessed: value, Modified: value, Changed: value})
	case uint8(wire.ClassFileEndOfFile):
		input, err = wire.EncodeFileEndOfFileInformation(wire.FileEndOfFileInformation{EndOfFile: 5000})
	case uint8(wire.ClassFileAllocation):
		input, err = wire.EncodeFileAllocationInformation(wire.FileAllocationInformation{AllocationSize: 4096})
	default:
		t.Fatalf("unexpected metadata class: %d", class)
	}
	if err != nil {
		t.Fatal(err)
	}
	body, err := wire.EncodeSetInfoRequest(wire.SetInfoRequest{
		ID: wire.FileID(f.open.ID), InfoType: wire.InfoFile, InfoClass: uint8(class), Input: input,
	})
	if err != nil {
		t.Fatal(err)
	}
	return wire.Message{Header: wire.Header{
		Command: wire.SetInfo, MessageID: f.nextID, SessionID: f.session.SessionID, TreeID: f.session.TreeID, CreditCharge: 1, Credit: 16,
	}, Body: body}
}

func receiveSetInfoAsync(ctx context.Context, t *testing.T, client *smbtest.Client) wire.Message {
	t.Helper()
	response, err := client.Receive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Messages) != 1 {
		t.Fatalf("expected one reply, got %+v", response.Messages)
	}
	return response.Messages[0]
}

func assertSetInfoAsyncEcho(t *testing.T, f *setInfoFixture, id uint64) {
	t.Helper()
	responses := exchange(f.ctx, t, f.client, sessionEcho(t, f.session, id))
	if len(responses) != 1 {
		t.Fatalf("ECHO replies: %+v", responses)
	}
	header := responses[0].Header
	if header.Command != wire.Echo || header.MessageID != id || header.SessionID != f.session.SessionID || header.Status != smb.StatusSuccess || header.Flags&wire.FlagAsync != 0 {
		t.Fatalf("extra reply or failed ECHO: %+v", header)
	}
}

func assertSetInfoQuiet(t *testing.T, f *setInfoFixture) {
	t.Helper()
	// Receive cancellation closes the client, so this is the last wire assertion.
	ctx, cancel := context.WithTimeout(f.ctx, 25*time.Millisecond)
	defer cancel()
	response, err := f.client.Receive(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("extra reply after completion: %+v, error %v", response.Messages, err)
	}
}

func TestSetInfoBufferedMetadataPendingIssue428(t *testing.T) {
	for _, test := range []struct {
		name  string
		class wire.FileInfoClass
	}{
		{"basic times", wire.ClassFileBasic},
		{"end of file", wire.ClassFileEndOfFile},
		{"allocation", wire.ClassFileAllocation},
	} {
		for _, fail := range []bool{false, true} {
			outcome := "success"
			var failure error
			if fail {
				outcome, failure = "error", smb.ErrIO
			}
			t.Run(test.name+"/"+outcome, func(t *testing.T) {
				checkSetInfoBufferedPending(t, test.class, failure)
			})
		}
	}
}

func checkSetInfoBufferedPending(t *testing.T, class wire.FileInfoClass, failure error) {
	t.Helper()
	f, storage := newBlockedSetInfoFixture(t, failure)
	data := bytes.Repeat([]byte("123456789"), 1000)
	f.write(t, string(data))
	before := f.attr(t)
	request := setInfoAsyncRequest(t, f, class)
	if err := f.client.Send(f.ctx, []wire.Message{request}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-storage.started:
	case <-f.ctx.Done():
		t.Fatal("SET_INFO did not reach storage")
	}
	pending := receiveSetInfoAsync(f.ctx, t, f.client)
	header := pending.Header
	if header.Command != wire.SetInfo || header.Status != smb.StatusPending || header.Flags&wire.FlagAsync == 0 || header.AsyncID == 0 || header.MessageID != request.Header.MessageID || header.SessionID != request.Header.SessionID || header.TreeID != 0 || header.CreditCharge != 1 || header.Credit != 16 {
		t.Fatalf("SET_INFO pending identity/credits: %+v", header)
	}
	assertSetInfoAsyncEcho(t, f, request.Header.MessageID+1)
	if after := f.attr(t); after != before {
		t.Fatalf("blocked SET_INFO changed metadata: before %+v, after %+v", before, after)
	}
	storage.unblock()
	final := receiveSetInfoAsync(f.ctx, t, f.client)
	header = final.Header
	want := smb.StatusSuccess
	if failure != nil {
		want = smb.StatusIODeviceError
	}
	if header.Command != wire.SetInfo || header.Status != want || header.Flags&wire.FlagAsync == 0 || header.MessageID != request.Header.MessageID || header.SessionID != request.Header.SessionID || header.AsyncID != pending.Header.AsyncID || header.TreeID != 0 || header.CreditCharge != 1 || header.Credit != 0 {
		t.Fatalf("SET_INFO final identity/credits: %+v, want status %#x", header, want)
	}
	if storage.calls.Load() != 1 {
		t.Fatalf("SetAttr calls = %d, want 1", storage.calls.Load())
	}
	if failure == nil {
		if _, err := wire.DecodeSetInfoResponse(final); err != nil {
			t.Fatal(err)
		}
		assertSetInfoBufferedResult(t, f, class, data)
	} else if after := f.attr(t); after != before {
		t.Fatalf("failed SET_INFO changed metadata: before %+v, after %+v", before, after)
	}
	assertSetInfoAsyncEcho(t, f, request.Header.MessageID+2)
	assertSetInfoQuiet(t, f)
}

func assertSetInfoBufferedResult(t *testing.T, f *setInfoFixture, class wire.FileInfoClass, data []byte) {
	t.Helper()
	// A later flush must not restore the buffered size or overwrite explicit times.
	if err := f.storage.Flush(f.ctx, f.open.Handle, smb.SyncData); err != nil {
		t.Fatal(err)
	}
	attr := f.attr(t)
	wantSize := uint64(len(data))
	switch uint8(class) {
	case uint8(wire.ClassFileBasic):
		want := time.Date(2001, 1, 2, 3, 4, 5, 0, time.UTC)
		if !attr.Accessed.Equal(want) || !attr.Modified.Equal(want) || !attr.Changed.Equal(want) {
			t.Fatalf("explicit times lost after flush: %+v", attr)
		}
	case uint8(wire.ClassFileEndOfFile):
		wantSize = 5000
	case uint8(wire.ClassFileAllocation):
		wantSize = 4096
	default:
		t.Fatalf("unexpected metadata class: %d", class)
	}
	if attr.Size != wantSize {
		t.Fatalf("EOF after SET_INFO and flush = %d, want %d", attr.Size, wantSize)
	}
	got := make([]byte, wantSize)
	count, err := f.storage.ReadAt(f.ctx, f.open.Handle, got, 0)
	if err != nil || count != len(got) || !bytes.Equal(got, data[:wantSize]) {
		t.Fatalf("retained data: count %d, error %v, matches %t", count, err, bytes.Equal(got, data[:wantSize]))
	}
}

func TestFastSetInfoAllocationStaysSynchronousIssue428(t *testing.T) {
	f := newSetInfoFixture(t, 2)
	f.write(t, "buffered data")
	before := f.attr(t)
	request := setInfoAsyncRequest(t, f, wire.ClassFileAllocation)
	responses := exchange(f.ctx, t, f.client, request)
	if len(responses) != 1 {
		t.Fatalf("local SET_INFO replies: %+v", responses)
	}
	response := responses[0]
	header := response.Header
	if header.Command != wire.SetInfo || header.Status != smb.StatusSuccess || header.Flags&wire.FlagAsync != 0 || header.MessageID != request.Header.MessageID || header.SessionID != request.Header.SessionID || header.TreeID != request.Header.TreeID || header.CreditCharge != 1 || header.Credit != 16 {
		t.Fatalf("local SET_INFO identity/credits: %+v", header)
	}
	if _, err := wire.DecodeSetInfoResponse(response); err != nil {
		t.Fatal(err)
	}
	if after := f.attr(t); after != before {
		t.Fatalf("allocation growth hint changed metadata: before %+v, after %+v", before, after)
	}
	assertSetInfoAsyncEcho(t, f, request.Header.MessageID+1)
	assertSetInfoQuiet(t, f)
}
