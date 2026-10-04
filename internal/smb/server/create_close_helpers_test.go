package server

import (
	"context"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/smbtest"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func newFileClient(t *testing.T) (*Server, *smbtest.Client, context.Context, smbtest.Session) {
	t.Helper()
	options := testOptions(t)
	options.Storage = newFilesMetaStorage(t)
	server, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	client, ctx, session := loginClient(t, server, smb.CipherAES128GCM, smb.SigningCMAC)
	return server, client, ctx, session
}

func fileCreate(ctx context.Context, t *testing.T, client *smbtest.Client, session smbtest.Session, id uint64, create wire.CreateRequest) wire.Message {
	t.Helper()
	body, err := wire.EncodeCreateRequest(create)
	if err != nil {
		t.Fatal(err)
	}
	message := wire.Message{Header: wire.Header{Command: wire.Create, MessageID: id, SessionID: session.SessionID, TreeID: session.TreeID, CreditCharge: 1, Credit: 1}, Body: body}
	return ioRoundTrip(ctx, t, client, message)
}

func fileClose(ctx context.Context, t *testing.T, client *smbtest.Client, session smbtest.Session, id uint64, fileID wire.FileID, flags uint16) wire.Message {
	t.Helper()
	body, err := wire.EncodeCloseRequest(wire.CloseRequest{ID: fileID, Flags: flags})
	if err != nil {
		t.Fatal(err)
	}
	message := wire.Message{Header: wire.Header{Command: wire.Close, MessageID: id, SessionID: session.SessionID, TreeID: session.TreeID, CreditCharge: 1, Credit: 1}, Body: body}
	return ioRoundTrip(ctx, t, client, message)
}

func createdFile(t *testing.T, message wire.Message) wire.CreateResponse {
	t.Helper()
	if message.Header.Status != smb.StatusSuccess {
		t.Fatalf("CREATE status = %#x", message.Header.Status)
	}
	response, err := wire.DecodeCreateResponse(message)
	if err != nil {
		t.Fatal(err)
	}
	return response
}
