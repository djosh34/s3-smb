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
	var responses []wire.Message
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
			if sendErr := connection.send(responses); sendErr != nil {
				return sendErr
			}
			responses = nil
			if sendErr := connection.sendPending(message.Header, operation); sendErr != nil {
				return sendErr
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
		responses = append(responses, response)
	}
	return connection.send(responses)
}

func (connection *connection) runMember(ctx context.Context, message wire.Message, rejection smb.Status, dependency *work, previous compoundState) (*work, bool, error) {
	if rejection != smb.StatusSuccess {
		return &work{result: reply{status: rejection}}, true, nil
	}
	if dependency != nil {
		// A related suffix owns its own pending identity and starts only after
		// its predecessor completes. Even a quick dependent reply stays async.
		return connection.startWork(ctx, message, dependency, previous), false, nil
	}
	if asyncEligible(message.Header.Command) && connection.server.handlers[message.Header.Command] != nil {
		operation := connection.startWork(ctx, message, nil, previous)
		completed, err := connection.waitLocal(operation)
		return operation, completed, err
	}
	return &work{result: connection.execute(ctx, message, previous)}, true, nil
}
