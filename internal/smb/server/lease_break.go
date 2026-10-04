package server

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/state"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

// BreakLeases is the CREATE seam: start other leases' breaks for the selected
// object, deliver their captured notifications, and wait for acknowledgment or
// revocation by ExpireBreaks. It runs cleanup through the normal close path.
// Cancellation stops the wait, not the breaks. No table lock spans I/O or a wait.
// An earlier pending break on the same object is also awaited.
func (server *Server) BreakLeases(ctx context.Context, object smb.ObjectKey, clientGUID, leaseKey state.GUID, target uint32) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	notifications, actions := server.options.State.BreakLeases(object, clientGUID, leaseKey, target)
	return server.deliverAndWaitLeaseBreaks(ctx, object, clientGUID, leaseKey, notifications, actions)
}

// deliverAndWaitLeaseBreaks reuses captured work selected under the table mutex.
func (server *Server) deliverAndWaitLeaseBreaks(ctx context.Context, object smb.ObjectKey, clientGUID, leaseKey state.GUID, notifications []state.Break, actions []state.CloseAction) error {
	if err := server.deliverLeaseBreaks(ctx, notifications, actions); err != nil {
		return err
	}
	for {
		changed := server.options.State.BreakChanges()
		if err := server.cleanup(context.WithoutCancel(ctx), server.options.State.CompleteDetachedBreaks(object, clientGUID, leaseKey)); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if !server.options.State.LeasesBreaking(object, clientGUID, leaseKey) {
			return nil
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// deliverLeaseBreaks sends captured stages but never waits for their next ACK.
func (server *Server) deliverLeaseBreaks(ctx context.Context, notifications []state.Break, actions []state.CloseAction) error {
	if err := server.cleanup(context.WithoutCancel(ctx), actions); err != nil {
		return err
	}
	for _, notification := range notifications {
		if notification.Binding.SessionID == 0 {
			if err := server.cleanup(context.WithoutCancel(ctx), server.options.State.CompleteDetachedBreak(notification)); err != nil {
				return err
			}
			continue
		}
		actions, err := server.sendLeaseBreak(ctx, notification)
		if cleanupErr := server.cleanup(context.WithoutCancel(ctx), actions); cleanupErr != nil {
			return cleanupErr
		}
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// A dropped holder cannot fail another client's CREATE. Its break
			// still needs acknowledgment, detached completion, or timer expiry.
			server.options.Logger.Debug("send lease break", "session_id", notification.Binding.SessionID, "error", err)
		}
	}
	return nil
}

func (server *Server) sessionConnection(id uint64) *connection {
	server.mu.Lock()
	defer server.mu.Unlock()
	return server.sessions[id]
}

// sendLeaseBreak leaves queued bytes with the ordered sender. Detached completion
// can end this wait before delivery; its caller owns the returned cleanup.
func (server *Server) sendLeaseBreak(ctx context.Context, notification state.Break) ([]state.CloseAction, error) {
	owner := server.sessionConnection(notification.Binding.SessionID)
	if owner == nil {
		return nil, errors.New("lease holder has no connection")
	}
	payload, err := owner.encodeLeaseBreak(notification)
	if err != nil {
		return nil, err
	}
	completed := owner.sender.enqueue(payload)
	for {
		changed := server.options.State.BreakChanges()
		if notification.AckRequired {
			actions := server.options.State.CompleteDetachedBreak(notification)
			if len(actions) != 0 || !server.options.State.BreakPending(notification) {
				return actions, nil
			}
		}
		select {
		case err := <-completed:
			return nil, err
		case <-changed:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// Notifications have SessionId zero. Encrypted sessions use the holder's keys;
// plaintext notifications stay unsigned per MS-SMB2 3.3.4.7. Signing their fixed
// MessageId with GMAC would reuse a nonce. They do not use replyProtection.
func (connection *connection) encodeLeaseBreak(notification state.Break) ([]byte, error) {
	connection.sessionMu.RLock()
	defer connection.sessionMu.RUnlock()
	session := connection.sessions[notification.Binding.SessionID]
	if session == nil || !session.active || session.protector == nil || session.identity.ClientGUID != notification.ClientGUID {
		return nil, errors.New("lease holder session is unavailable")
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
		return handleClassicOplockBreak(request, message)
	}
	ack, err := wire.DecodeLeaseBreakRequest(message)
	if err != nil {
		return reply{}, err
	}
	notifications, actions, status := request.Opens.AckBreak(state.Binding{SessionID: request.Session.SessionID}, request.Session.ClientGUID, state.GUID(ack.Key), ack.State)
	if status != smb.StatusSuccess {
		return reply{status: status}, nil
	}
	if deliveryErr := request.server.deliverLeaseBreaks(ctx, notifications, actions); deliveryErr != nil {
		return reply{}, fmt.Errorf("lease acknowledgment follow-up: %w", deliveryErr)
	}
	body, err := wire.EncodeLeaseBreakResponse(wire.LeaseBreakResponse{Key: ack.Key, State: ack.State})
	return reply{body: body}, err
}

func handleClassicOplockBreak(request RequestContext, message wire.Message) (reply, error) {
	ack, err := wire.DecodeOplockBreakRequest(message)
	if err != nil {
		return reply{}, err
	}
	id, status := request.FileID(ack.ID)
	if status != smb.StatusSuccess {
		return reply{status: status}, nil
	}
	owner := request.server.sessionConnection(request.Session.SessionID)
	if owner == nil {
		return reply{status: smb.StatusUserSessionDeleted}, nil
	}
	owner.sessionMu.RLock()
	session := owner.sessions[request.Session.SessionID]
	treeExists := false
	if session != nil && session.active {
		_, treeExists = session.trees[message.Header.TreeID]
	}
	owner.sessionMu.RUnlock()
	if !treeExists {
		return reply{status: smb.StatusNetworkNameDeleted}, nil
	}
	_, status = request.Opens.Find(state.FileID{Persistent: id.Persistent, Volatile: id.Volatile}, state.Binding{SessionID: request.Session.SessionID, TreeID: message.Header.TreeID})
	if status != smb.StatusSuccess {
		return reply{status: status}, nil
	}
	// No open holds a classic oplock, so its state is never Breaking.
	return reply{status: smb.StatusInvalidDeviceState, fileID: id}, nil
}
