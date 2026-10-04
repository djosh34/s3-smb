package server

import (
	"context"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/smbtest"
	"github.com/djosh34/s3-smb/internal/smb/state"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func notifyServer(t *testing.T, cipher uint16) *Server {
	t.Helper()
	options := testOptions(t)
	options.Storage = &cleanupStorage{}
	if cipher == 0 {
		options.Encryption = AllowPlaintext
	}
	server, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	return server
}

func insertNotifyDirectory(t *testing.T, server *Server, session smbtest.Session) state.Open {
	t.Helper()
	object := smb.ObjectKey{Inode: 2}
	reservation, status := server.options.State.Reserve(state.OpenRequest{
		Object: object, Binding: state.Binding{SessionID: session.SessionID, TreeID: session.TreeID},
		User: server.options.Account.User, Share: server.options.ShareName, ClientGUID: state.GUID{2},
		GrantedAccess: 1, Sharing: 7,
	})
	if status != smb.StatusSuccess {
		t.Fatal(status)
	}
	open, status := server.options.State.Commit(reservation, state.Grant{Handle: cleanupHandle{object: object}, Directory: true})
	if status != smb.StatusSuccess {
		t.Fatal(status)
	}
	return open
}

func notifyMessage(t *testing.T, open state.Open, id uint64) wire.Message {
	t.Helper()
	body, err := wire.EncodeChangeNotifyRequest(wire.ChangeNotifyRequest{
		ID: wire.FileID{Persistent: open.ID.Persistent, Volatile: open.ID.Volatile}, OutputLength: 4096, Filter: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	return wire.Message{Header: wire.Header{
		Command: wire.ChangeNotify, MessageID: id, SessionID: open.Binding.SessionID,
		TreeID: open.Binding.TreeID, CreditCharge: 1, Credit: 1,
	}, Body: body}
}

func checkNotifyRefusal(t *testing.T, response, request wire.Message, status smb.Status) {
	t.Helper()
	header := response.Header
	if header.Command != wire.ChangeNotify || header.Status != status || header.MessageID != request.Header.MessageID ||
		header.SessionID != request.Header.SessionID || header.TreeID != request.Header.TreeID ||
		header.Flags&wire.FlagResponse == 0 || header.Flags&wire.FlagAsync != 0 || header.AsyncID != 0 ||
		header.CreditCharge != request.Header.CreditCharge || header.Credit != max(uint16(1), request.Header.Credit) {
		t.Fatalf("CHANGE_NOTIFY refusal: %+v", header)
	}
	body, err := wire.DecodeErrorResponse(response)
	if err != nil {
		t.Fatal(err)
	}
	if body.ContextCount != 0 || len(body.Data) != 0 {
		t.Fatalf("CHANGE_NOTIFY error body: %+v", body)
	}
}

func checkNotifyOpen(t *testing.T, server *Server, open state.Open) {
	t.Helper()
	current, status := server.options.State.Find(open.ID, open.Binding)
	if status != smb.StatusSuccess || !reflect.DeepEqual(current, open) {
		t.Fatalf("CHANGE_NOTIFY changed the open: %+v, %v", current, status)
	}
}

func checkNotifyState(t *testing.T, server *Server, open state.Open) {
	t.Helper()
	checkNotifyOpen(t, server, open)
	server.mu.Lock()
	defer server.mu.Unlock()
	for connection := range server.connections {
		connection.pendingMu.Lock()
		count := len(connection.pending)
		connection.pendingMu.Unlock()
		if count != 0 {
			t.Errorf("CHANGE_NOTIFY kept %d pending requests", count)
		}
	}
}

func checkFollowingEcho(ctx context.Context, t *testing.T, client *smbtest.Client, session smbtest.Session, id uint64) {
	t.Helper()
	response := exchange(ctx, t, client, sessionEcho(t, session, id))
	if len(response) != 1 || response[0].Header.Command != wire.Echo || response[0].Header.MessageID != id || response[0].Header.Status != smb.StatusSuccess {
		t.Fatalf("extra reply before following ECHO: %+v", response)
	}
}

func notifyProtectionName(cipher uint16) string {
	if cipher == 0 {
		return "signed"
	}
	return "encrypted"
}

// Regression for #107: neither request can replace the other's identity or own
// pending work, even if someone registers a CHANGE_NOTIFY handler.
func TestChangeNotifySameDirectoryHandle(t *testing.T) {
	for _, cipher := range []uint16{0, smb.CipherAES256GCM} {
		t.Run(notifyProtectionName(cipher), func(t *testing.T) {
			server := notifyServer(t, cipher)
			var calls atomic.Int32
			server.handlers[wire.ChangeNotify] = func(context.Context, RequestContext, wire.Message) (reply, error) {
				calls.Add(1)
				return reply{status: smb.StatusSuccess}, nil
			}
			if asyncEligible(wire.ChangeNotify) {
				t.Fatal("CHANGE_NOTIFY is async-eligible")
			}
			client, ctx, session := loginClient(t, server, cipher, smb.SigningGMAC)
			open := insertNotifyDirectory(t, server, session)
			requests := []wire.Message{notifyMessage(t, open, session.NextMessageID), notifyMessage(t, open, session.NextMessageID+1)}
			requests[0].Header.Credit, requests[1].Header.Credit = 0, 7
			sent := make(chan error, 1)
			go func() {
				for _, request := range requests {
					if err := client.Send(ctx, []wire.Message{request}); err != nil {
						sent <- err
						return
					}
				}
				sent <- nil
			}()
			for _, request := range requests {
				response, err := client.Receive(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if len(response.Messages) != 1 {
					t.Fatalf("CHANGE_NOTIFY reply members: %d", len(response.Messages))
				}
				checkNotifyRefusal(t, response.Messages[0], request, smb.StatusNotSupported)
			}
			if err := <-sent; err != nil {
				t.Fatal(err)
			}
			checkFollowingEcho(ctx, t, client, session, session.NextMessageID+2)
			checkNotifyState(t, server, open)
			if calls.Load() != 0 {
				t.Fatal("CHANGE_NOTIFY ran a registered handler")
			}
		})
	}
}

// Regression for #146: both compound orders get one complete frame. No pending
// reply or delayed completion can resend the earlier compound before ECHO.
func TestChangeNotifyCompoundDoesNotResend(t *testing.T) {
	for _, cipher := range []uint16{0, smb.CipherAES256GCM} {
		t.Run(notifyProtectionName(cipher), func(t *testing.T) {
			for _, notifyFirst := range []bool{false, true} {
				name := "echo_first"
				if notifyFirst {
					name = "notify_first"
				}
				t.Run(name, func(t *testing.T) { checkNotifyCompound(t, cipher, notifyFirst) })
			}
		})
	}
}

func checkNotifyCompound(t *testing.T, cipher uint16, notifyFirst bool) {
	t.Helper()
	server := notifyServer(t, cipher)
	client, ctx, session := loginClient(t, server, cipher, smb.SigningGMAC)
	open := insertNotifyDirectory(t, server, session)
	notify := notifyMessage(t, open, session.NextMessageID)
	echo := sessionEcho(t, session, session.NextMessageID)
	echo.Header.TreeID = session.TreeID
	requests := []wire.Message{echo, notify}
	if notifyFirst {
		requests = []wire.Message{notify, echo}
	}
	requests[1].Header.MessageID++
	requests[1].Header.Flags = wire.FlagRelated
	requests[1].Header.SessionID, requests[1].Header.TreeID = ^uint64(0), ^uint32(0)
	responses := exchange(ctx, t, client, requests...)
	if len(responses) != 2 {
		t.Fatalf("compound reply members: %d", len(responses))
	}
	for index, response := range responses {
		request := requests[index]
		request.Header.SessionID, request.Header.TreeID = session.SessionID, session.TreeID
		if response.Header.Command != request.Header.Command || response.Header.MessageID != request.Header.MessageID || response.Header.Flags&wire.FlagAsync != 0 || response.Header.Credit != 1 {
			t.Fatalf("compound member %d: %+v", index, response.Header)
		}
		if response.Header.Command == wire.ChangeNotify {
			checkNotifyRefusal(t, response, request, smb.StatusNotSupported)
		} else if response.Header.Status != smb.StatusSuccess {
			t.Fatalf("compound ECHO: %+v", response.Header)
		}
	}
	if responses[0].Header.NextCommand == 0 || responses[1].Header.NextCommand != 0 {
		t.Fatal("compound response links are wrong")
	}
	checkFollowingEcho(ctx, t, client, session, session.NextMessageID+2)
	checkNotifyState(t, server, open)
}

func TestChangeNotifyChecksSessionAndTree(t *testing.T) {
	server := notifyServer(t, 0)
	client, ctx := corePipeClient(t, server)
	exchange(ctx, t, client, negotiateMessage(t, 3))
	session := smbtest.Session{SessionID: 77, TreeID: 12}
	open := insertNotifyDirectory(t, server, session)
	for index, status := range []smb.Status{smb.StatusUserSessionDeleted, smb.StatusNetworkNameDeleted, smb.StatusNotSupported} {
		request := notifyMessage(t, open, uint64(index+1))
		switch index {
		case 0:
			request.Header.SessionID++
		case 1:
			request.Header.TreeID++
		}
		responses := exchange(ctx, t, client, request)
		if len(responses) != 1 {
			t.Fatalf("CHANGE_NOTIFY reply members: %d", len(responses))
		}
		checkNotifyRefusal(t, responses[0], request, status)
	}
	checkNotifyState(t, server, open)
}

func TestRelatedChangeNotifyFollowsPendingPredecessor(t *testing.T) {
	server := notifyServer(t, smb.CipherAES256GCM)
	release := make(chan struct{})
	server.handlers[wire.Read] = func(ctx context.Context, _ RequestContext, _ wire.Message) (reply, error) {
		select {
		case <-release:
			body, err := wire.EncodeReadResponse(wire.ReadResponse{Data: []byte("x")})
			return reply{body: body}, err
		case <-ctx.Done():
			return reply{}, ctx.Err()
		}
	}
	client, ctx, session := loginClient(t, server, smb.CipherAES256GCM, smb.SigningGMAC)
	open := insertNotifyDirectory(t, server, session)
	read := treeRequest(t, session, session.NextMessageID, wire.Read)
	notify := notifyMessage(t, open, session.NextMessageID+1)
	notify.Header.Flags = wire.FlagRelated
	notify.Header.SessionID, notify.Header.TreeID = ^uint64(0), ^uint32(0)
	if err := client.Send(ctx, []wire.Message{read, notify}); err != nil {
		t.Fatal(err)
	}
	pending := make(map[uint64]uint64)
	for index := range 2 {
		response, err := client.Receive(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(response.Messages) != 1 {
			t.Fatalf("pending reply members: %d", len(response.Messages))
		}
		header := response.Messages[0].Header
		if header.MessageID != session.NextMessageID+uint64(index) || header.Status != smb.StatusPending || header.Flags&wire.FlagAsync == 0 || header.Credit == 0 || header.AsyncID == 0 {
			t.Fatalf("related pending: %+v", header)
		}
		pending[header.MessageID] = header.AsyncID
	}
	if pending[read.Header.MessageID] == pending[notify.Header.MessageID] {
		t.Fatal("related CHANGE_NOTIFY shares its predecessor's async ID")
	}
	checkFollowingEcho(ctx, t, client, session, session.NextMessageID+2)
	close(release)
	for range 2 {
		response, err := client.Receive(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(response.Messages) != 1 {
			t.Fatalf("final reply members: %d", len(response.Messages))
		}
		message := response.Messages[0]
		header := message.Header
		asyncID, exists := pending[header.MessageID]
		if !exists || header.AsyncID != asyncID || header.SessionID != session.SessionID || header.Flags&wire.FlagAsync == 0 || header.Credit != 0 {
			t.Fatalf("related final: %+v", header)
		}
		delete(pending, header.MessageID)
		if header.MessageID == notify.Header.MessageID {
			if header.Status != smb.StatusNotSupported {
				t.Fatalf("related CHANGE_NOTIFY status: %+v", header)
			}
			if _, err := wire.DecodeErrorResponse(message); err != nil {
				t.Fatal(err)
			}
		} else if header.Status != smb.StatusSuccess {
			t.Fatalf("predecessor status: %+v", header)
		}
	}
	checkFollowingEcho(ctx, t, client, session, session.NextMessageID+3)
	checkNotifyOpen(t, server, open)
}
