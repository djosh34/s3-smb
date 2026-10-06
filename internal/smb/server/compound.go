package server

import (
	"context"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

// compoundState is copied for each member. Async work publishes it with its reply.
type compoundState struct {
	fileID wire.FileID
	status smb.Status
}

func (previous compoundState) after(result reply) compoundState {
	if result.fileID != (wire.FileID{}) {
		previous.fileID = result.fileID
	}
	previous.status = result.status
	return previous
}

// Dispatch does not read FileIds from bodies. These commands need a FileId;
// CREATE does not. Related members inherit error-severity predecessor statuses.
func needsFileID(command wire.Command) bool {
	switch uint16(command) {
	case uint16(wire.Close), uint16(wire.Read), uint16(wire.Write), uint16(wire.Flush), uint16(wire.Lock), uint16(wire.IOCTL),
		uint16(wire.QueryDirectory), uint16(wire.ChangeNotify), uint16(wire.QueryInfo), uint16(wire.SetInfo):
		return true
	default:
		return false
	}
}

func (connection *connection) process(ctx context.Context, messages []wire.Message, denied bool) error {
	validationErr := validateCompound(messages)
	if err := connection.credits.consume(messages); err != nil {
		return err
	}
	rejection := smb.StatusSuccess
	if validationErr != nil {
		connection.server.options.Logger.Info("compound refused", "reason", validationErr)
		rejection = smb.StatusInvalidParameter
	}
	if denied {
		rejection = smb.StatusAccessDenied
	}
	replies := compoundReplies{connection: connection}
	defer replies.startHeld()
	var preceding wire.Header
	var prerequisite *work
	var previous compoundState
	chainRejection := rejection
	for index, message := range messages {
		dependency := (*work)(nil)
		if index > 0 && message.Header.Flags&wire.FlagRelated != 0 {
			message.Header.SessionID, message.Header.TreeID = preceding.SessionID, preceding.TreeID
			dependency = prerequisite
		} else {
			previous = compoundState{}
			chainRejection = rejection
		}
		if index == 0 && message.Header.Flags&wire.FlagRelated != 0 && rejection == smb.StatusSuccess {
			chainRejection = smb.StatusInvalidParameter
		}
		preceding = message.Header
		if message.Header.Command == wire.Cancel {
			if chainRejection == smb.StatusSuccess {
				connection.cancelPending(message.Header)
			}
			continue
		}
		prerequisite = nil
		operation, completed, err := connection.runMember(ctx, message, chainRejection, dependency, previous, index == 0)
		if err != nil {
			return err
		}
		if !completed {
			if err = replies.pending(index, message.Header, operation); err != nil {
				return err
			}
			prerequisite = operation
			continue
		}
		result := operation.result
		previous = previous.after(result)
		response, err := makeResponse(message.Header, result, connection.credits.grant(message.Header))
		if err != nil {
			return err
		}
		preceding = response.Header
		replies.add(response)
	}
	return connection.send(replies.responses)
}

// compoundReplies collects the replies of one compound. When the first member
// goes async, macOS takes the other replies only in one chain after its final
// reply: until a compound reply has come back split, it reads each packet as
// the reply to a whole compound. So the rest get no interim of their own and
// are held for that chain. A later member that goes async splits the replies,
// and each member after it is answered on its own.
type compoundReplies struct {
	connection *connection
	first      *pendingRequest
	responses  []wire.Message
	held       []heldReply
}

// pending answers a member that did not complete in time.
func (replies *compoundReplies) pending(index int, header wire.Header, operation *work) error {
	if replies.first != nil {
		// Without an interim, CANCEL names the member by message ID, or the
		// whole rest of the compound by the first member's async ID.
		replies.connection.pendingMu.Lock()
		replies.connection.pending[header.MessageID] = &pendingRequest{work: operation, header: header}
		replies.first.held = append(replies.first.held, operation)
		replies.connection.pendingMu.Unlock()
		replies.held = append(replies.held, heldReply{work: operation, header: header, credits: replies.connection.credits.grant(header)})
		return nil
	}
	if err := replies.connection.send(replies.responses); err != nil {
		return err
	}
	replies.responses = nil
	pending, err := replies.connection.sendPending(header, operation)
	if err != nil {
		return err
	}
	if index == 0 {
		replies.first = pending
	} else {
		go replies.connection.complete(pending, nil)
	}
	return nil
}

func (replies *compoundReplies) add(response wire.Message) {
	if replies.first != nil {
		replies.held = append(replies.held, heldReply{response: &response})
		return
	}
	replies.responses = append(replies.responses, response)
}

// startHeld sends the chain after an async first member once its work ends.
// sendPending counted a worker for it, so process calls this on every return.
func (replies *compoundReplies) startHeld() {
	if replies.first != nil {
		go replies.connection.complete(replies.first, replies.held)
	}
}

// runMember runs one member, async if it may wait on storage. CLOSE waits
// for the open's pending requests, which can wait on S3, so it goes async too,
// but only as the first member: macOS cannot read a compound whose CLOSE
// comes back split. A request that gets no reply, final or interim, within 2
// minutes fails on macOS, and its data is lost.
func (connection *connection) runMember(ctx context.Context, message wire.Message, rejection smb.Status, dependency *work, previous compoundState, first bool) (*work, bool, error) {
	if rejection != smb.StatusSuccess {
		return &work{result: reply{status: rejection}}, true, nil
	}
	if dependency != nil {
		// A related suffix owns its own pending identity and starts only after
		// its predecessor completes. Even a quick dependent reply stays async.
		return connection.startWork(ctx, message, dependency, previous), false, nil
	}
	command := message.Header.Command
	if (asyncEligible(command) || first && command == wire.Close) && connection.server.handlers[command] != nil {
		operation := connection.startWork(ctx, message, nil, previous)
		completed, err := connection.waitLocal(operation)
		return operation, completed, err
	}
	return &work{result: connection.execute(ctx, message, previous)}, true, nil
}
