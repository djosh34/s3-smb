package smbtest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

// NewClient rejects a nil connection and takes ownership of conn on success.
func NewClient(conn net.Conn) (*Client, error) {
	if conn == nil {
		return nil, errors.New("smbtest: nil connection")
	}
	return &Client{conn: conn, pending: make(map[uint64]uint64)}, nil
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
	length := uint32(len(payload))
	frame := make([]byte, 4, 4+len(payload))
	frame[1] = byte(length >> 16)
	frame[2] = byte(length >> 8 & 0xff)
	frame[3] = byte(length & 0xff)
	return client.SendRaw(ctx, append(frame, payload...))
}

// SendRaw sends framed bytes unchanged, even if their framing or body is invalid.
// Concurrent sends are serialized. An I/O error closes the connection, since a
// partial frame cannot be retried safely.
func (client *Client) SendRaw(ctx context.Context, framed []byte) error {
	client.sendMu.Lock()
	defer client.sendMu.Unlock()
	return client.transfer(ctx, client.conn.SetWriteDeadline, func() error {
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

// Receive returns one frame without consuming a pending reply's final result.
// An async final must match a previously received pending reply by both IDs.
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
			client.pending[h.MessageID] = h.AsyncID
			continue
		}
		asyncID, exists := client.pending[h.MessageID]
		if !exists || asyncID != h.AsyncID {
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
	err := client.transfer(ctx, client.conn.SetReadDeadline, func() error {
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

// transfer gives each direction its own deadline and cancellation callback.
// It joins the callback before resetting the deadline for the next operation.
func (client *Client) transfer(ctx context.Context, deadline func(time.Time) error, operation func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	limit, _ := ctx.Deadline()
	if err := deadline(limit); err != nil {
		return errors.Join(err, client.Close())
	}
	canceled := make(chan error, 1)
	stop := context.AfterFunc(ctx, func() {
		err := deadline(time.Now())
		if err != nil {
			err = errors.Join(err, client.Close())
		}
		canceled <- err
	})
	err := operation()
	if !stop() {
		err = errors.Join(err, <-canceled)
	}
	if ctx.Err() != nil {
		err = errors.Join(err, ctx.Err())
	}
	err = errors.Join(err, deadline(time.Time{}))
	if err != nil {
		return errors.Join(err, client.Close())
	}
	return nil
}

// Close closes the owned connection once and returns its cleanup error.
// It unblocks any send or receive in progress.
func (client *Client) Close() error {
	client.closeOnce.Do(func() { client.closeErr = client.conn.Close() })
	return client.closeErr
}
