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

func TestLateSessionCompletionDuringCleanup(t *testing.T) {
	for _, cleanup := range []struct {
		name    string
		command wire.Command
	}{
		{name: "logoff", command: wire.Logoff},
		{name: "tree_disconnect", command: wire.TreeDisconnect},
	} {
		for _, protection := range []struct {
			name    string
			cipher  uint16
			signing uint16
		}{
			{name: "cmac", signing: smb.SigningCMAC},
			{name: "gmac", signing: smb.SigningGMAC},
			{name: "gcm128", cipher: smb.CipherAES128GCM, signing: smb.SigningGMAC},
			{name: "gcm256", cipher: smb.CipherAES256GCM, signing: smb.SigningGMAC},
		} {
			for _, outcome := range []struct {
				name   string
				status smb.Status
			}{
				{name: "canceled", status: smb.StatusCancelled},
				{name: "late_success", status: smb.StatusSuccess},
				{name: "late_error", status: smb.StatusIODeviceError},
			} {
				t.Run(fmt.Sprintf("%s/%s/%s", cleanup.name, protection.name, outcome.name), func(t *testing.T) {
					checkLateSessionCleanup(t, cleanup.command, protection.cipher, protection.signing, outcome.status)
				})
			}
		}
	}
}

func checkLateSessionCleanup(t *testing.T, command wire.Command, cipher, signing uint16, wantStatus smb.Status) {
	t.Helper()
	options := testOptions(t)
	if cipher == 0 {
		options.Encryption = AllowPlaintext
	}
	storage := &cleanupStorage{}
	options.Storage = storage
	server, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	release := newAsyncGate()
	canceled := make(chan struct{})
	closedWhileActive := make(chan int32, 1)
	body, err := wire.EncodeReadResponse(wire.ReadResponse{Data: []byte("L")})
	if err != nil {
		t.Fatal(err)
	}
	server.handlers[wire.Read] = func(ctx context.Context, _ RequestContext, _ wire.Message) (reply, error) {
		<-ctx.Done()
		close(canceled)
		<-release.done
		closedWhileActive <- storage.closed.Load()
		if wantStatus == smb.StatusCancelled {
			return reply{}, ctx.Err()
		}
		if wantStatus == smb.StatusIODeviceError {
			return reply{}, smb.ErrIO
		}
		return reply{body: body}, nil
	}
	client, ctx, session := loginClient(t, server, cipher, signing)
	owner := onlyConnection(t, server)
	// Release the handler before the pipe fixture tries to drain it on failure.
	t.Cleanup(release.release)
	open := insertSessionOpen(t, server, session, false, 50)
	request := treeRequest(t, session, session.NextMessageID, wire.Read)
	if err := client.Send(ctx, []wire.Message{request}); err != nil {
		t.Fatal(err)
	}
	pending := receiveLateSessionReply(ctx, t, client, cipher)
	if pending.Header.Status != smb.StatusPending || pending.Header.Flags&wire.FlagAsync == 0 || pending.Header.AsyncID == 0 || pending.Header.MessageID != request.Header.MessageID || pending.Header.SessionID != session.SessionID || pending.Header.Credit == 0 {
		t.Fatalf("pending: %+v", pending.Header)
	}
	cleanup := treeRequest(t, session, session.NextMessageID+1, command)
	if err := client.Send(ctx, []wire.Message{cleanup}); err != nil {
		t.Fatal(err)
	}
	waitAsyncSignal(ctx, t, canceled)
	if storage.closed.Load() != 0 {
		t.Fatal("cleanup closed storage before the canceled handler returned")
	}
	if _, status := options.State.Find(open.ID, open.Binding); status != smb.StatusSuccess {
		t.Fatal("cleanup removed the active handler's open before draining it")
	}
	owner.sessionMu.RLock()
	entry := owner.sessions[session.SessionID]
	removed := entry == nil
	if command == wire.TreeDisconnect && entry != nil {
		_, exists := entry.trees[session.TreeID]
		removed = !exists
	}
	retained := owner.replyProtection[request.Header.MessageID].session != nil
	owner.sessionMu.RUnlock()
	if !removed || !retained {
		t.Fatal("cleanup did not retire the identity while retaining the pending reply keys")
	}
	release.release()
	checkLateSessionReplies(ctx, t, client, cipher, pending.Header, cleanup.Header, wantStatus)
	if storage.closed.Load() != 1 {
		t.Fatalf("cleanup closed %d handles, want 1", storage.closed.Load())
	}
	if closed := <-closedWhileActive; closed != 0 {
		t.Fatalf("handler used storage after cleanup closed %d handles", closed)
	}
	if _, status := options.State.Find(open.ID, open.Binding); status == smb.StatusSuccess {
		t.Fatal("cleanup left the retired identity's open")
	}
	drained := make(chan struct{})
	go func() {
		owner.workers.Wait()
		close(drained)
	}()
	waitAsyncSignal(ctx, t, drained)
	owner.sessionMu.RLock()
	keys, holders := len(owner.replyProtection), len(owner.inflight)
	owner.sessionMu.RUnlock()
	owner.pendingMu.Lock()
	pendingCount := len(owner.pending)
	owner.pendingMu.Unlock()
	if keys != 0 || holders != 0 || pendingCount != 0 {
		t.Fatalf("terminal reply retained work or keys: keys %d, holders %d, pending %d", keys, holders, pendingCount)
	}
	checkRetiredSessionTraffic(ctx, t, client, session, command, cipher)
}

// Cleanup drains the handler, not the sender. Either reply can arrive first.
func checkLateSessionReplies(ctx context.Context, t *testing.T, client *smbtest.Client, cipher uint16, pending, cleanup wire.Header, wantStatus smb.Status) {
	t.Helper()
	seen := make(map[uint64]bool)
	for range 2 {
		response := receiveLateSessionReply(ctx, t, client, cipher)
		header := response.Header
		if seen[header.MessageID] {
			t.Fatalf("duplicate reply: %+v", header)
		}
		seen[header.MessageID] = true
		switch header.MessageID {
		case pending.MessageID:
			if header.Command != wire.Read || header.Status != wantStatus || header.SessionID != pending.SessionID || header.AsyncID != pending.AsyncID || header.Flags&wire.FlagAsync == 0 || header.TreeID != 0 || header.Credit != 0 || header.CreditCharge != pending.CreditCharge {
				t.Fatalf("terminal reply: %+v", header)
			}
			if wantStatus == smb.StatusSuccess {
				read, err := wire.DecodeReadResponse(response)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(read.Data, []byte("L")) {
					t.Fatalf("late result changed: %q", read.Data)
				}
			}
		case cleanup.MessageID:
			if header.Command != cleanup.Command || header.Status != smb.StatusSuccess || header.SessionID != cleanup.SessionID || header.TreeID != cleanup.TreeID || header.Flags&wire.FlagAsync != 0 {
				t.Fatalf("cleanup reply: %+v", header)
			}
		default:
			t.Fatalf("unexpected reply: %+v", header)
		}
	}
}

func receiveLateSessionReply(ctx context.Context, t *testing.T, client *smbtest.Client, cipher uint16) wire.Message {
	t.Helper()
	response, err := client.Receive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Messages) != 1 {
		t.Fatalf("expected one reply, got %d", len(response.Messages))
	}
	message := response.Messages[0]
	encrypted := bytes.HasPrefix(response.Raw, []byte{0xfd, 'S', 'M', 'B'})
	if encrypted != (cipher != 0) {
		t.Fatal("reply lost its negotiated protection")
	}
	if encrypted && (message.Header.Flags&wire.FlagSigned != 0 || message.Header.Signature != [16]byte{}) {
		t.Fatal("encrypted reply was separately signed")
	}
	if !encrypted && message.Header.Status != smb.StatusPending && message.Header.Flags&wire.FlagSigned == 0 {
		t.Fatal("terminal plaintext reply was not signed")
	}
	return message
}

func checkRetiredSessionTraffic(ctx context.Context, t *testing.T, client *smbtest.Client, session smbtest.Session, command wire.Command, cipher uint16) {
	t.Helper()
	stale := treeRequest(t, session, session.NextMessageID+2, wire.Read)
	if command == wire.TreeDisconnect {
		if err := client.Send(ctx, []wire.Message{stale}); err != nil {
			t.Fatal(err)
		}
		response := receiveLateSessionReply(ctx, t, client, cipher)
		if response.Header.MessageID != stale.Header.MessageID || response.Header.Status != smb.StatusNetworkNameDeleted {
			t.Fatalf("retired tree reply or duplicate completion: %+v", response.Header)
		}
		response = exchange(ctx, t, client, sessionEcho(t, session, session.NextMessageID+3))[0]
		if response.Header.Status != smb.StatusSuccess || response.Header.MessageID != session.NextMessageID+3 {
			t.Fatalf("surviving session ECHO: %+v", response.Header)
		}
		return
	}
	// A removed session cannot decrypt new traffic. Raw plaintext probes must
	// receive unprotected refusals, not replies made with its retired keys.
	for _, request := range []wire.Message{stale, echo(t, session.NextMessageID+3)} {
		payload, err := wire.Join([]wire.Message{request})
		if err != nil {
			t.Fatal(err)
		}
		sendPayload(ctx, t, client, payload)
		payload, err = client.ReceiveRaw(ctx)
		if err != nil {
			t.Fatal(err)
		}
		messages, err := wire.Split(payload)
		if err != nil {
			t.Fatalf("removed session keys protected new traffic: %v", err)
		}
		if len(messages) != 1 {
			t.Fatalf("unexpected replies after cleanup: %d", len(messages))
		}
		header := messages[0].Header
		want := smb.StatusUserSessionDeleted
		if request.Header.Command == wire.Echo {
			want = smb.StatusSuccess
		}
		if header.MessageID != request.Header.MessageID || header.Command != request.Header.Command || header.SessionID != request.Header.SessionID || header.Status != want || header.Flags&(wire.FlagSigned|wire.FlagAsync) != 0 || header.Signature != [16]byte{} {
			t.Fatalf("retired identity reply or duplicate completion: %+v", header)
		}
	}
}
