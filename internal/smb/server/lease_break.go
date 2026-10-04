package server

import (
	"context"
	"encoding/binary"
	"errors"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/state"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

// breakLease starts the break, sends its notification and waits until the
// holder acknowledges it, closes its last open of the file, or the scavenger
// revokes the lease after LeaseBreakTimeout. Cancellation ends the wait, not
// the break.
func (server *Server) breakLease(ctx context.Context, pending leaseBreak) error {
	notifications, actions := server.options.State.BreakLease(pending.object, pending.client, pending.key, pending.target)
	if err := server.cleanup(ctx, actions); err != nil {
		return err
	}
	var sent <-chan error
	for _, notification := range notifications {
		sent = server.sendLeaseBreak(notification)
	}
	for {
		changed := server.options.State.BreakChanges()
		if !server.options.State.LeaseBreaking(pending.object, pending.client, pending.key) {
			return nil
		}
		select {
		case err := <-sent:
			// A holder that cannot be told still times out.
			if err != nil {
				server.options.Logger.Info("send lease break", "error", err)
			}
			sent = nil
		case <-changed:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// sendLeaseBreak queues the notification on the holder's connection and
// returns the result of sending it.
func (server *Server) sendLeaseBreak(notification state.Break) <-chan error {
	result := make(chan error, 1)
	server.mu.Lock()
	owner := server.sessions[notification.Binding.SessionID]
	server.mu.Unlock()
	if owner == nil {
		result <- errors.New("lease holder has no connection")
		return result
	}
	payload, err := owner.encodeLeaseBreak(notification)
	if err != nil {
		result <- err
		return result
	}
	return owner.sender.enqueue(payload)
}

// encodeLeaseBreak protects the notification for the holder's session.
// Notifications have SessionId zero. Plaintext ones stay unsigned
// (MS-SMB2 3.3.4.7): signing their fixed MessageId with GMAC would reuse a
// nonce.
func (connection *connection) encodeLeaseBreak(notification state.Break) ([]byte, error) {
	connection.sessionMu.RLock()
	defer connection.sessionMu.RUnlock()
	session, _, status := connection.identify(wire.Header{Command: wire.OplockBreak, SessionID: notification.Binding.SessionID})
	if status != smb.StatusSuccess {
		return nil, errors.New("lease holder session is gone")
	}
	flags := uint32(0)
	if notification.AckRequired {
		flags = 1
	}
	body, err := wire.EncodeLeaseBreakNotification(wire.LeaseBreakNotification{
		Key: [16]byte(notification.LeaseKey), Epoch: notification.Epoch, Flags: flags,
		CurrentState: notification.CurrentState, NewState: notification.NewState,
	})
	if err != nil {
		return nil, err
	}
	header := wire.Header{Command: wire.OplockBreak, MessageID: ^uint64(0), Flags: wire.FlagResponse}
	payload, err := wire.Join([]wire.Message{{Header: header, Body: body}})
	if err != nil {
		return nil, err
	}
	if session.identity.Encrypted {
		return session.protector.Seal(payload)
	}
	return payload, nil
}

// classicOplockBreak tells an oplock acknowledgment (24 bytes) from a lease
// acknowledgment (36 bytes).
func classicOplockBreak(message wire.Message) bool {
	return len(message.Body) >= 2 && binary.LittleEndian.Uint16(message.Body[:2]) == 24
}

func validateOplockBreak(message wire.Message) error {
	if classicOplockBreak(message) {
		_, err := wire.DecodeOplockBreakRequest(message)
		return err
	}
	_, err := wire.DecodeLeaseBreakRequest(message)
	return err
}

func handleOplockBreak(ctx context.Context, request RequestContext, message wire.Message) (reply, error) {
	if classicOplockBreak(message) {
		// Oplocks are never granted, so no open has an oplock break to
		// acknowledge (MS-SMB2 3.3.5.22.1).
		return reply{status: smb.StatusInvalidDeviceState}, nil
	}
	ack, err := wire.DecodeLeaseBreakRequest(message)
	if err != nil {
		return reply{}, err
	}
	actions, status := request.Opens.AckBreak(request.Session.ClientGUID, state.GUID(ack.Key), ack.State)
	if err = request.Cleanup(ctx, actions); err != nil {
		return reply{}, err
	}
	if status != smb.StatusSuccess {
		return reply{status: status}, nil
	}
	body, err := wire.EncodeLeaseBreakResponse(wire.LeaseBreakResponse{Key: ack.Key, State: ack.State})
	return reply{body: body}, err
}
