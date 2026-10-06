package server

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

const localWait = 5 * time.Millisecond

// work publishes one immutable result before closing done. Dependent members
// wait for that publication, rather than for transport completion.
type work struct {
	done     chan struct{}
	cancel   context.CancelFunc
	result   reply
	compound compoundState
}

type pendingRequest struct {
	work *work
	// held is the rest of the compound after an async first member. The
	// client has only the first member's async ID, so CANCEL with it stops
	// them too. Guarded by pendingMu.
	held    []*work
	header  wire.Header
	asyncID uint64
}

func asyncEligible(command wire.Command) bool {
	return command == wire.Create || command == wire.Read || command == wire.Write || command == wire.Flush || command == wire.SetInfo
}

func (connection *connection) execute(ctx context.Context, message wire.Message, previous compoundState) reply {
	if err := ctx.Err(); err != nil {
		return reply{status: smb.StatusFromError(err)}
	}
	if message.Header.Flags&wire.FlagRelated != 0 && needsFileID(message.Header.Command) && previous.status&0xc0000000 == 0xc0000000 {
		return reply{status: previous.status}
	}
	result, err := connection.dispatch(ctx, message, previous)
	// Storage may fail a cancelled call with ErrIO, which a handler may turn
	// into a status. A cancelled request that fails answers STATUS_CANCELLED.
	if ctxErr := ctx.Err(); ctxErr != nil && (err != nil || result.status&0xc0000000 == 0xc0000000) {
		err = errors.Join(ctxErr, err)
	}
	if err != nil {
		status := smb.StatusFromError(err)
		level, text := slog.LevelDebug, "request refused"
		if errors.Is(err, context.Canceled) {
			text = "request canceled"
		} else if serverFault(status) {
			level, text = slog.LevelError, "request failed"
		}
		connection.server.options.Logger.Log(ctx, level, text, "command", message.Header.Command, "message_id", message.Header.MessageID, "error", err)
		return reply{status: status, fileID: result.fileID}
	}
	return result
}

// serverFault reports whether a failed request is the server's fault rather
// than an answer to what the client asked, such as a name that is not there.
func serverFault(status smb.Status) bool {
	return status == smb.StatusInternalError || status == smb.StatusIODeviceError || status == smb.StatusIOTimeout ||
		status == smb.StatusInsufficientResources || status == smb.StatusDiskFull
}

func (connection *connection) startWork(ctx context.Context, message wire.Message, prerequisite *work, previous compoundState) *work {
	ctx, cancel := context.WithCancel(ctx)
	operation := &work{done: make(chan struct{}), cancel: cancel}
	connection.workers.Add(1)
	go func() {
		defer connection.workers.Done()
		defer close(operation.done)
		defer cancel()
		if prerequisite != nil {
			select {
			case <-prerequisite.done:
				previous = prerequisite.compound
			case <-ctx.Done():
				operation.result.status = smb.StatusCancelled
				operation.compound = previous.after(operation.result)
				return
			}
		}
		started := time.Now()
		operation.result = connection.execute(ctx, message, previous)
		operation.compound = previous.after(operation.result)
		// Temporary for #620.
		if took := time.Since(started); took > 30*time.Second {
			connection.server.options.Logger.Info("slow request", "took", took, "command", message.Header.Command, "message_id", message.Header.MessageID, "async", true)
		}
	}()
	return operation
}

func (connection *connection) waitLocal(operation *work) (bool, error) {
	timer := time.NewTimer(localWait)
	defer timer.Stop()
	select {
	case <-operation.done:
		return true, nil
	case <-timer.C:
		return false, nil
	case <-connection.ctx.Done():
		return false, connection.ctx.Err()
	}
}

func asyncResponse(request wire.Header, result reply, asyncID uint64, credits uint16) (wire.Message, error) {
	message, err := makeResponse(request, result, credits)
	if err != nil {
		return wire.Message{}, err
	}
	message.Header.Flags = wire.FlagResponse | wire.FlagAsync
	message.Header.AsyncID = asyncID
	message.Header.ProcessID, message.Header.TreeID = 0, 0
	return message, nil
}

// sendPending registers operation for CANCEL and sends its interim reply. The
// caller starts the goroutine that sends the final reply.
func (connection *connection) sendPending(header wire.Header, operation *work) (*pendingRequest, error) {
	pending := &pendingRequest{work: operation, header: header, asyncID: connection.nextAsyncID}
	connection.nextAsyncID++
	connection.pendingMu.Lock()
	connection.pending[header.MessageID] = pending
	connection.pendingMu.Unlock()
	message, err := asyncResponse(header, reply{status: smb.StatusPending}, pending.asyncID, connection.credits.grant(header))
	if err != nil {
		return nil, err
	}
	if err := connection.send([]wire.Message{message}); err != nil {
		return nil, err
	}
	connection.workers.Add(1)
	return pending, nil
}

// heldReply is a compound member answered in the chain after an async first
// member: a response, or work still running and the credits already granted.
type heldReply struct {
	response *wire.Message
	work     *work
	header   wire.Header
	credits  uint16
}

// complete sends the final reply of pending, followed in one chain by the
// replies of the rest of its compound.
func (connection *connection) complete(pending *pendingRequest, held []heldReply) {
	defer connection.workers.Done()
	defer func() {
		connection.pendingMu.Lock()
		delete(connection.pending, pending.header.MessageID)
		for _, member := range held {
			if member.work != nil {
				delete(connection.pending, member.header.MessageID)
			}
		}
		connection.pendingMu.Unlock()
	}()
	works := []*work{pending.work}
	for _, member := range held {
		if member.work != nil {
			works = append(works, member.work)
		}
	}
	for _, operation := range works {
		select {
		case <-connection.ctx.Done():
			return
		case <-operation.done:
		}
	}
	// Both cases can be ready when late work finishes after a disconnect.
	if connection.ctx.Err() != nil {
		return
	}
	// Final success and error share this path, with identity saved at pending.
	// They never call the credit allocator or reuse the interim buffer.
	message, err := asyncResponse(pending.header, pending.work.result, pending.asyncID, 0)
	messages := []wire.Message{message}
	for _, member := range held {
		if err != nil {
			break
		}
		if member.response != nil {
			messages = append(messages, *member.response)
			continue
		}
		message, err = makeResponse(member.header, member.work.result, member.credits)
		messages = append(messages, message)
	}
	if err == nil {
		err = connection.send(messages)
	}
	if err != nil {
		connection.server.options.Logger.Error("async reply failed", "error", errors.Join(err, connection.close()))
	}
}

func (connection *connection) cancelPending(header wire.Header) {
	connection.pendingMu.Lock()
	defer connection.pendingMu.Unlock()
	var pending *pendingRequest
	if header.Flags&wire.FlagAsync != 0 {
		for _, candidate := range connection.pending {
			if candidate.asyncID == header.AsyncID {
				pending = candidate
				break
			}
		}
	} else {
		pending = connection.pending[header.MessageID]
	}
	if pending != nil && pending.header.SessionID == header.SessionID {
		pending.work.cancel()
		for _, held := range pending.held {
			held.cancel()
		}
	}
}
