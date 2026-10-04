package server

import (
	"context"
	"errors"
	"log/slog"
	"math"
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
	work    *work
	header  wire.Header
	asyncID uint64
}

func asyncEligible(command wire.Command) bool {
	return command == wire.Create || command == wire.Read || command == wire.Write || command == wire.Flush
}

func (connection *connection) execute(ctx context.Context, message wire.Message, previous compoundState) reply {
	if err := ctx.Err(); err != nil {
		return reply{status: smb.StatusFromError(err)}
	}
	if message.Header.Flags&wire.FlagRelated != 0 && needsFileID(message.Header.Command) && previous.status != smb.StatusSuccess {
		return reply{status: previous.status}
	}
	result, err := connection.dispatch(ctx, message, previous)
	if err != nil {
		level, text := slog.LevelError, "request failed"
		if errors.Is(err, context.Canceled) {
			level, text = slog.LevelDebug, "request canceled"
		}
		connection.server.options.Logger.Log(ctx, level, text, "command", message.Header.Command, "message_id", message.Header.MessageID, "error", err)
		return reply{status: smb.StatusFromError(err)}
	}
	return result
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
		operation.result = connection.execute(ctx, message, previous)
		operation.compound = previous.after(operation.result)
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

func (connection *connection) sendPending(header wire.Header, operation *work) error {
	if connection.nextAsyncID == math.MaxUint64 {
		return errors.New("async ID space exhausted")
	}
	pending := &pendingRequest{work: operation, header: header, asyncID: connection.nextAsyncID}
	connection.nextAsyncID++
	connection.pendingMu.Lock()
	connection.pending[header.MessageID] = pending
	connection.pendingMu.Unlock()
	message, err := asyncResponse(header, reply{status: smb.StatusPending}, pending.asyncID, connection.credits.grant(header))
	if err != nil {
		return err
	}
	if err := connection.send([]wire.Message{message}); err != nil {
		return err
	}
	connection.workers.Add(1)
	go connection.complete(pending)
	return nil
}

func (connection *connection) complete(pending *pendingRequest) {
	defer connection.workers.Done()
	defer func() {
		connection.pendingMu.Lock()
		delete(connection.pending, pending.header.MessageID)
		connection.pendingMu.Unlock()
	}()
	select {
	case <-connection.ctx.Done():
		return
	case <-pending.work.done:
	}
	// Both cases can be ready when late work finishes after a disconnect.
	if connection.ctx.Err() != nil {
		return
	}
	// Final success and error share this path, with identity saved at pending.
	// They never call the credit allocator or reuse the interim buffer.
	message, err := asyncResponse(pending.header, pending.work.result, pending.asyncID, 0)
	if err == nil {
		err = connection.send([]wire.Message{message})
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
	}
}
