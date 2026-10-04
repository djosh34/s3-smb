package smbtest

import (
	"context"
	"errors"
	"fmt"
	"net"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

// RetainedOpen supplies the identities and CREATE fields of one detached open.
// Lease is the last granted V2 lease, including its key and epoch. Request holds
// the original path and access fields, without lease or durable contexts.
type RetainedOpen struct {
	Request    wire.CreateRequest
	Lease      wire.LeaseContext
	ID         wire.FileID
	CreateGUID [16]byte
	ClientGUID [16]byte
}

// Reconnect owns a fresh connection, logs in with PreviousSessionID, connects
// the share and sends DH2C with RqLs for each retained open in order. It reuses
// the previous client GUID and selected algorithms, never its protection keys.
// Options supplies the same account and share. Returned results contain the new
// FileIDs; session contains the next unused message ID and remaining credits.
// On any failure it closes the new client and returns no partial results.
func Reconnect(ctx context.Context, conn net.Conn, previous Session, options LoginOptions, opens []RetainedOpen) (*Client, Session, []CreateResult, error) {
	client, err := NewClient(conn)
	if err != nil {
		return nil, Session{}, nil, err
	}
	session, results, err := client.reconnect(ctx, previous, options, opens)
	if err != nil {
		return nil, Session{}, nil, errors.Join(err, client.Close())
	}
	return client, session, results, nil
}

func (client *Client) reconnect(ctx context.Context, previous Session, options LoginOptions, opens []RetainedOpen) (Session, []CreateResult, error) {
	if previous.SessionID == 0 || previous.ClientGUID == [16]byte{} {
		return Session{}, nil, errors.New("smbtest: reconnect requires a previous session and client GUID")
	}
	for _, open := range opens {
		if open.ClientGUID != previous.ClientGUID {
			return Session{}, nil, errors.New("smbtest: retained open client GUID differs from previous session")
		}
		if _, err := createRequest(reconnectCreate(open)); err != nil {
			return Session{}, nil, err
		}
	}
	options.PreviousSessionID = previous.SessionID
	options.ClientGUID = previous.ClientGUID
	options.Signing = previous.Signing
	options.Cipher = previous.Cipher
	session, err := client.Login(ctx, options)
	if err != nil {
		return Session{}, nil, err
	}
	results := make([]CreateResult, 0, len(opens))
	for index, open := range opens {
		result, err := client.reconnectOpen(ctx, &session, open)
		if err != nil {
			return Session{}, nil, fmt.Errorf("smbtest: reconnect open %d: %w", index, err)
		}
		results = append(results, result)
	}
	return session, results, nil
}

func reconnectCreate(open RetainedOpen) CreateOptions {
	return CreateOptions{Request: open.Request, Lease: &open.Lease, Reconnect: &wire.DurableReconnect{ID: open.ID, CreateGUID: open.CreateGUID}}
}

func (client *Client) reconnectOpen(ctx context.Context, session *Session, open RetainedOpen) (CreateResult, error) {
	if session.Credits == 0 {
		return CreateResult{}, errors.New("smbtest: no credit for reconnect CREATE")
	}
	header := wire.Header{Command: wire.Create, MessageID: session.NextMessageID, SessionID: session.SessionID, TreeID: session.TreeID, CreditCharge: 1, Credit: 16}
	if err := client.SendCreate(ctx, header, reconnectCreate(open)); err != nil {
		return CreateResult{}, err
	}
	for {
		reply, err := client.Receive(ctx)
		if err != nil {
			return CreateResult{}, err
		}
		if len(reply.Messages) != 1 {
			return CreateResult{}, errors.New("smbtest: compounded reconnect reply")
		}
		message := reply.Messages[0]
		if message.Header.Command != wire.Create || message.Header.MessageID != header.MessageID || message.Header.SessionID != header.SessionID || message.Header.Flags&wire.FlagAsync == 0 && message.Header.TreeID != header.TreeID {
			return CreateResult{}, errors.New("smbtest: reconnect reply identity mismatch")
		}
		if message.Header.Status == smb.StatusPending {
			session.Credits = session.Credits - 1 + message.Header.Credit
			continue
		}
		if message.Header.Flags&wire.FlagAsync == 0 {
			session.Credits = session.Credits - 1 + message.Header.Credit
		}
		session.NextMessageID++
		return DecodeCreateReply(message)
	}
}
