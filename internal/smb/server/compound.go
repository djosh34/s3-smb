package server

import (
	"context"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func (connection *connection) process(ctx context.Context, messages []wire.Message) error {
	validationErr := validateCompound(messages)
	if err := connection.credits.consume(messages); err != nil {
		return err
	}
	if validationErr != nil {
		connection.server.options.Logger.Info("compound refused", "reason", validationErr)
	}
	var responses []wire.Message
	var preceding wire.Header
	var prerequisite *work
	for _, message := range messages {
		dependency := (*work)(nil)
		if message.Header.Flags&wire.FlagRelated != 0 {
			message.Header.SessionID, message.Header.TreeID = preceding.SessionID, preceding.TreeID
			dependency = prerequisite
		}
		preceding = message.Header
		if message.Header.Command == wire.Cancel {
			if validationErr == nil {
				connection.cancelPending(message.Header)
			}
			continue
		}
		result := reply{status: smb.StatusInvalidParameter}
		prerequisite = nil
		if validationErr == nil {
			operation, completed, err := connection.runMember(ctx, message, dependency)
			if err != nil {
				return err
			}
			if !completed {
				if err := connection.send(responses); err != nil {
					return err
				}
				responses = nil
				if err := connection.sendPending(message.Header, operation); err != nil {
					return err
				}
				prerequisite = operation
				continue
			}
			result = operation.result
		}
		response, err := makeResponse(message.Header, result, connection.credits.grant(message.Header))
		if err != nil {
			return err
		}
		preceding = response.Header
		responses = append(responses, response)
	}
	return connection.send(responses)
}

func (connection *connection) runMember(ctx context.Context, message wire.Message, dependency *work) (*work, bool, error) {
	if dependency != nil {
		// A related suffix owns its own pending identity and starts only after
		// its predecessor completes. Even a quick dependent reply stays async.
		return connection.startWork(ctx, message, dependency), false, nil
	}
	if asyncEligible(message.Header.Command) && connection.server.handlers[message.Header.Command] != nil {
		operation := connection.startWork(ctx, message, nil)
		completed, err := connection.waitLocal(operation)
		return operation, completed, err
	}
	return &work{result: connection.execute(ctx, message)}, true, nil
}
