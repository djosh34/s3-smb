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
		operation, completed, err := connection.runMember(ctx, message, chainRejection, dependency, previous)
		if err != nil {
			return err
		}
		if !completed {
			if err = replies.pending(message.Header, operation); err != nil {
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
		replies.add(message.Header, result, response)
	}
	return connection.send(replies.responses)
}

// compoundReplies collects the replies of one compound. Until a compound
// reply has come back split, macOS reads each packet as the reply to a whole
// compound, and it fails a CLOSE that comes back split. So a compound is
// never split: when any member goes async, the first answered member gets the
// interim reply, and its final reply and those of the rest follow in one
// chain. The rest get no interim of their own.
type compoundReplies struct {
	connection *connection
	first      *pendingRequest
	responses  []wire.Message
	held       []heldReply
	// head is the request and result of the first answered member, which
	// takes the interim reply when a later member goes async.
	headResult reply
	head       wire.Header
}

// pending answers a member that did not complete in time.
func (replies *compoundReplies) pending(header wire.Header, operation *work) error {
	if replies.first == nil && len(replies.responses) > 0 {
		// An async reply has no tree ID, so a TREE_CONNECT head keeps its
		// reply and the compound splits after it.
		if replies.headResult.treeID != 0 {
			if err := replies.connection.send(replies.responses); err != nil {
				return err
			}
			replies.responses = nil
		} else if err := replies.holdAnswered(); err != nil {
			return err
		}
	}
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
	pending, err := replies.connection.sendPending(header, operation, replies.connection.credits.grant(header))
	if err != nil {
		return err
	}
	replies.first = pending
	return nil
}

// holdAnswered turns the members answered so far into a held chain: the
// first gets the interim reply, with the credits its reply would have
// granted, and the rest wait for the chain.
func (replies *compoundReplies) holdAnswered() error {
	done := make(chan struct{})
	close(done)
	answered := &work{done: done, result: replies.headResult, cancel: func() {}}
	pending, err := replies.connection.sendPending(replies.head, answered, replies.responses[0].Header.Credit)
	if err != nil {
		return err
	}
	replies.first = pending
	for _, response := range replies.responses[1:] {
		replies.held = append(replies.held, heldReply{response: &response})
	}
	replies.responses = nil
	return nil
}

func (replies *compoundReplies) add(request wire.Header, result reply, response wire.Message) {
	if replies.first != nil {
		replies.held = append(replies.held, heldReply{response: &response})
		return
	}
	if len(replies.responses) == 0 {
		replies.head, replies.headResult = request, result
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

// runMember runs one member, async if it may wait on storage. A request that
// gets no reply, final or interim, within 2 minutes fails on macOS, and its
// data is lost, so nothing that runs inline may wait on S3.
func (connection *connection) runMember(ctx context.Context, message wire.Message, rejection smb.Status, dependency *work, previous compoundState) (*work, bool, error) {
	if rejection != smb.StatusSuccess {
		return &work{result: reply{status: rejection}}, true, nil
	}
	if dependency != nil {
		// A related suffix owns its own pending identity and starts only after
		// its predecessor completes. Even a quick dependent reply stays async.
		return connection.startWork(ctx, message, dependency, previous), false, nil
	}
	if asyncEligible(message.Header.Command) {
		operation := connection.startWork(ctx, message, nil, previous)
		completed, err := connection.waitLocal(operation)
		return operation, completed, err
	}
	return &work{result: connection.execute(ctx, message, previous)}, true, nil
}
