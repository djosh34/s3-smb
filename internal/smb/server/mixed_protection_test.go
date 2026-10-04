package server

import (
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/crypt"
	"github.com/djosh34/s3-smb/internal/smb/smbtest"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func TestPlaintextCompoundAcrossEncryptedSessionsGetsDeniedReplies(t *testing.T) {
	server, err := New(testOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	client := newRawSessions(t, server)
	first := client.start(t, 0)
	firstKey := client.finish(t, first, 0)
	second := client.start(t, 0)
	secondKey := client.finish(t, second, 0)
	messages := []wire.Message{
		sessionEcho(t, smbtest.Session{SessionID: first.id}, client.nextID),
		sessionEcho(t, smbtest.Session{SessionID: second.id}, client.nextID+1),
	}
	if err := client.client.Send(client.ctx, messages); err != nil {
		t.Fatal(err)
	}
	client.nextID += 2
	for index, protector := range []*crypt.Protector{firstKey, secondKey} {
		payload, err := client.client.ReceiveRaw(client.ctx)
		if err != nil {
			t.Fatal(err)
		}
		payload, err = protector.Open(payload)
		if err != nil {
			t.Fatal(err)
		}
		members, err := wire.Split(payload)
		if err != nil {
			t.Fatal(err)
		}
		if len(members) != 1 || members[0].Header.Status != smb.StatusAccessDenied || members[0].Header.SessionID != messages[index].Header.SessionID {
			t.Fatal("mixed-session denial lost protection or identity")
		}
	}
	if response := client.protected(t, secondKey, sessionEcho(t, smbtest.Session{SessionID: second.id}, 0)); response.Header.Status != smb.StatusSuccess {
		t.Fatal("mixed-session denial closed the transport")
	}
}
