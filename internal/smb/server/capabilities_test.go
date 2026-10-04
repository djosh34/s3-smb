package server

import (
	"context"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/smbtest"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

type capabilityClient struct {
	client  *smbtest.Client
	ctx     context.Context
	session smbtest.Session
	next    uint64
}

func newCapabilityServer(t *testing.T) *Server {
	t.Helper()
	options := testOptions(t)
	options.Storage = newFilesMetaStorage(t)
	server, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	return server
}

func capabilityConnection(t *testing.T, server *Server) *capabilityClient {
	t.Helper()
	client, ctx, session := loginClient(t, server, smb.CipherAES128GCM, smb.SigningCMAC)
	return &capabilityClient{client: client, ctx: ctx, session: session, next: session.NextMessageID}
}

func (client *capabilityClient) exchange(t *testing.T, command wire.Command, body []byte, status smb.Status) wire.Message {
	t.Helper()
	message := wire.Message{Header: wire.Header{
		Command: command, MessageID: client.next, CreditCharge: 1, Credit: 1,
		SessionID: client.session.SessionID, TreeID: client.session.TreeID,
	}, Body: body}
	client.next++
	responses := exchange(client.ctx, t, client.client, message)
	if len(responses) != 1 || responses[0].Header.Status != status {
		t.Fatalf("%v response = %+v, want status %#x", command, responses, status)
	}
	return responses[0]
}

func (client *capabilityClient) create(t *testing.T, request wire.CreateRequest, status smb.Status) wire.CreateResponse {
	t.Helper()
	body, err := wire.EncodeCreateRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	response := client.exchange(t, wire.Create, body, status)
	if status != smb.StatusSuccess {
		return wire.CreateResponse{}
	}
	created, err := wire.DecodeCreateResponse(response)
	if err != nil {
		t.Fatal(err)
	}
	return created
}

func (client *capabilityClient) close(t *testing.T, id wire.FileID) {
	t.Helper()
	body, err := wire.EncodeCloseRequest(wire.CloseRequest{ID: id})
	if err != nil {
		t.Fatal(err)
	}
	response := client.exchange(t, wire.Close, body, smb.StatusSuccess)
	if _, err := wire.DecodeCloseResponse(response); err != nil {
		t.Fatal(err)
	}
}

func (client *capabilityClient) fileInformation(t *testing.T, id wire.FileID, class wire.FileInfoClass) []byte {
	t.Helper()
	body, err := wire.EncodeQueryInfoRequest(wire.QueryInfoRequest{
		ID: id, InfoType: wire.InfoFile, InfoClass: uint8(class), OutputLength: 4096,
	})
	if err != nil {
		t.Fatal(err)
	}
	response := client.exchange(t, wire.QueryInfo, body, smb.StatusSuccess)
	data, err := wire.DecodeQueryInfoResponse(response)
	if err != nil {
		t.Fatal(err)
	}
	return data.Data
}

func (client *capabilityClient) filesystemAttributes(t *testing.T) uint32 {
	t.Helper()
	root := client.create(t, wire.CreateRequest{
		Disposition: fileOpen, Options: fileDirectoryFile, ShareAccess: 7, DesiredAccess: 0x00120089,
	}, smb.StatusSuccess)
	body, err := wire.EncodeQueryInfoRequest(wire.QueryInfoRequest{
		ID: root.ID, InfoType: wire.InfoFilesystem, InfoClass: uint8(wire.ClassFilesystemAttribute), OutputLength: 4096,
	})
	if err != nil {
		t.Fatal(err)
	}
	response := client.exchange(t, wire.QueryInfo, body, smb.StatusSuccess)
	data, err := wire.DecodeQueryInfoResponse(response)
	if err != nil {
		t.Fatal(err)
	}
	attributes, err := wire.DecodeFilesystemAttributeInformation(data.Data)
	if err != nil {
		t.Fatal(err)
	}
	client.close(t, root.ID)
	return attributes.Attributes
}

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
