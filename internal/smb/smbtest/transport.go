package smbtest

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

// NewClient rejects a nil connection and takes ownership of conn on success.
func NewClient(conn net.Conn) (*Client, error) {
	if conn == nil {
		return nil, errors.New("smbtest: nil connection")
	}
	return &Client{conn: conn, pending: make(map[uint64]pendingReply), sendSlot: make(chan struct{}, 1)}, nil
}

// Send encodes one compound, changing only NextCommand links and padding.
// It does not allocate message IDs or adjust credits, flags or signatures.
func (client *Client) Send(ctx context.Context, messages []wire.Message) error {
	payload, err := wire.Join(messages)
	if err != nil {
		return err
	}
	if len(payload) > 0xffffff {
		return errors.New("smbtest: frame exceeds 24-bit length")
	}
	frame := make([]byte, 4, 4+len(payload))
	binary.BigEndian.PutUint32(frame, uint32(len(payload)&0xffffff))
	return client.SendRaw(ctx, append(frame, payload...))
}

// SendRaw sends framed bytes unchanged, even if their framing or body is invalid.
// Concurrent sends are serialized. An I/O error closes the connection, since a
// partial frame cannot be retried safely.
func (client *Client) SendRaw(ctx context.Context, framed []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case client.sendSlot <- struct{}{}:
		defer func() { <-client.sendSlot }()
	case <-ctx.Done():
		return ctx.Err()
	}
	return client.transfer(ctx, func() error {
		for len(framed) > 0 {
			n, err := client.conn.Write(framed)
			if err != nil {
				return err
			}
			if n == 0 {
				return io.ErrNoProgress
			}
			framed = framed[n:]
		}
		return nil
	})
}

// Receive returns the next frame. An interim reply is returned on its own, and
// its final reply comes from a later call. An async final must keep the pending
// reply's MessageID, AsyncID and SessionID.
// A correlation error returns the decoded reply as well, for test assertions.
// Callers must use only one receiver, including ReceiveRaw.
func (client *Client) Receive(ctx context.Context) (Reply, error) {
	payload, err := client.ReceiveRaw(ctx)
	if err != nil {
		return Reply{}, err
	}
	messages, err := wire.Split(payload)
	reply := Reply{Raw: payload, Messages: messages}
	if err != nil {
		return reply, err
	}
	for _, message := range messages {
		h := message.Header
		if h.Flags&wire.FlagAsync == 0 {
			if h.Status == smb.StatusPending {
				return reply, errors.New("smbtest: pending reply lacks async flag")
			}
			if _, pending := client.pending[h.MessageID]; pending {
				return reply, fmt.Errorf("smbtest: final reply for message %d lacks async flag", h.MessageID)
			}
			continue
		}
		if h.Status == smb.StatusPending {
			if _, exists := client.pending[h.MessageID]; exists {
				return reply, fmt.Errorf("smbtest: duplicate pending reply for message %d", h.MessageID)
			}
			client.pending[h.MessageID] = pendingReply{asyncID: h.AsyncID, sessionID: h.SessionID}
			continue
		}
		pending, exists := client.pending[h.MessageID]
		if !exists || pending.asyncID != h.AsyncID || pending.sessionID != h.SessionID {
			return reply, fmt.Errorf("smbtest: unmatched async final for message %d, async %d", h.MessageID, h.AsyncID)
		}
		delete(client.pending, h.MessageID)
	}
	return reply, nil
}

// ReceiveRaw reads a direct TCP frame and returns its exact payload. It does not
// decode SMB headers or track pending replies. It may run concurrently with Send.
func (client *Client) ReceiveRaw(ctx context.Context) ([]byte, error) {
	var payload []byte
	err := client.transfer(ctx, func() error {
		var header [4]byte
		if _, err := io.ReadFull(client.conn, header[:]); err != nil {
			return err
		}
		if header[0] != 0 {
			return errors.New("smbtest: invalid direct TCP frame type")
		}
		length := int(header[1])<<16 | int(header[2])<<8 | int(header[3])
		payload = make([]byte, length)
		_, err := io.ReadFull(client.conn, payload)
		return err
	})
	if err != nil {
		return nil, err
	}
	return payload, nil
}

// transfer closes the connection when active I/O is canceled. Close waits for
// the cancellation callback's cleanup and returns any error it recorded.
func (client *Client) transfer(ctx context.Context, operation func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	stop := context.AfterFunc(ctx, client.closeTransport)
	err := operation()
	stopped := stop()
	err = errors.Join(err, ctx.Err())
	if !stopped || err != nil {
		return errors.Join(err, client.Close())
	}
	return nil
}

func (client *Client) closeTransport() {
	client.closeOnce.Do(func() { client.closeErr = client.conn.Close() })
}

// Close closes the owned connection once and returns its cleanup error.
// It unblocks any send or receive in progress.
func (client *Client) Close() error {
	client.closeTransport()
	return client.closeErr
}
