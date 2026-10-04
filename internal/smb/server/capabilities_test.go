package server

import (
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func TestAAPLReplyOnEveryConnection(t *testing.T) {
	server := newCapabilityServer(t)
	query, err := wire.EncodeAAPLQuery(wire.AAPLQuery{Requested: 7, ClientCapabilities: ^uint64(0)})
	if err != nil {
		t.Fatal(err)
	}
	for connection := range 2 {
		client := capabilityConnection(t, server)
		for range 2 {
			created := client.create(t, wire.CreateRequest{
				Disposition: fileOpen, Options: fileDirectoryFile, ShareAccess: 7,
				DesiredAccess: 0x00120089, Contexts: []wire.CreateContext{query},
			}, smb.StatusSuccess)
			if len(created.Contexts) != 1 {
				t.Fatalf("connection %d: contexts = %+v", connection, created.Contexts)
			}
			reply, err := wire.DecodeAAPLReply(created.Contexts[0])
			if err != nil {
				t.Fatal(err)
			}
			if reply != (wire.AAPLReply{Returned: 7, ServerCapabilities: 0, VolumeCapabilities: 0x06, Model: "s3-smb"}) {
				t.Fatalf("connection %d: AAPL = %+v", connection, reply)
			}
		}
		if err := client.client.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestFailedAAPLCreateDoesNotNegotiate(t *testing.T) {
	server := newCapabilityServer(t)
	client := capabilityConnection(t, server)
	server.mu.Lock()
	connection := server.sessions[client.session.SessionID]
	server.mu.Unlock()
	if connection == nil {
		t.Fatal("authenticated connection missing")
	}
	query, err := wire.EncodeAAPLQuery(wire.AAPLQuery{Requested: 7})
	if err != nil {
		t.Fatal(err)
	}
	request := wire.CreateRequest{
		Name: "missing", Disposition: fileOpen, DesiredAccess: fileAllAccess,
		ShareAccess: 7, Contexts: []wire.CreateContext{query},
	}
	client.create(t, request, smb.StatusObjectNameNotFound)
	if connection.aapl.Load() {
		t.Fatal("failed CREATE negotiated AAPL without a reply")
	}
	request.Disposition = fileCreateDisposition
	created := client.create(t, request, smb.StatusSuccess)
	if len(created.Contexts) != 1 || !connection.aapl.Load() {
		t.Fatal("successful CREATE did not negotiate AAPL")
	}
	client.close(t, created.ID)
	other := capabilityConnection(t, server)
	server.mu.Lock()
	otherConnection := server.sessions[other.session.SessionID]
	server.mu.Unlock()
	if otherConnection == nil || otherConnection.aapl.Load() {
		t.Fatal("AAPL negotiation leaked to a new connection")
	}
}

func TestMalformedAAPLDoesNotCreateFile(t *testing.T) {
	server := newCapabilityServer(t)
	client := capabilityConnection(t, server)
	client.create(t, wire.CreateRequest{
		Name: "invalid-aapl", Disposition: fileCreateDisposition, ShareAccess: 7, DesiredAccess: fileAllAccess,
		Contexts: []wire.CreateContext{{Name: "AAPL", Data: []byte{1}}},
	}, smb.StatusInvalidParameter)
	resolved, err := server.options.Storage.Lookup(t.Context(), "invalid-aapl")
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Exists {
		t.Fatal("malformed AAPL created a file")
	}
	client.exchange(t, wire.Echo, echo(t, 0).Body, smb.StatusSuccess)
}
