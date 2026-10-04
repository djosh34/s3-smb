package smbtest

import (
	"context"
	"errors"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func isLeaseBreak(header wire.Header) bool {
	return header.Command == wire.OplockBreak && header.MessageID == ^uint64(0) &&
		header.SessionID == 0 && header.Flags&wire.FlagResponse != 0 &&
		header.Flags&wire.FlagAsync == 0 && header.Status == smb.StatusSuccess
}

func (client *Client) routeLeaseBreaks(reply *Reply) error {
	var notifications []wire.LeaseBreakNotification
	var messages []wire.Message
	for _, message := range reply.Messages {
		if message.Header.MessageID != ^uint64(0) {
			messages = append(messages, message)
			continue
		}
		if !isLeaseBreak(message.Header) {
			return errors.New("smbtest: invalid lease break identity")
		}
		notification, err := wire.DecodeLeaseBreakNotification(message)
		if err != nil {
			return err
		}
		notifications = append(notifications, notification)
	}
	client.leaseBreaks = append(client.leaseBreaks, notifications...)
	reply.Messages = messages
	return nil
}

// WaitLeaseBreak returns the next V2 notification. It verifies protection just
// like Receive and queues normal replies, including pending and final replies,
// for later Receive calls. Only one receiver may run at a time. Send may run
// concurrently, so a test can acknowledge a break while Receive waits for CREATE.
// Canceling active I/O closes the connection, as it does for Receive.
func (client *Client) WaitLeaseBreak(ctx context.Context) (wire.LeaseBreakNotification, error) {
	if err := ctx.Err(); err != nil {
		return wire.LeaseBreakNotification{}, err
	}
	for len(client.leaseBreaks) == 0 {
		reply, err := client.receiveRouted(ctx)
		if err != nil {
			return wire.LeaseBreakNotification{}, err
		}
		if len(reply.Messages) != 0 {
			client.replies = append(client.replies, reply)
		}
	}
	notification := client.leaseBreaks[0]
	client.leaseBreaks[0] = wire.LeaseBreakNotification{}
	client.leaseBreaks = client.leaseBreaks[1:]
	return notification, nil
}

// SendLeaseBreakAcknowledgment sends a lease acknowledgment with the supplied
// session, tree, message ID and credits. Receive returns its normal reply.
// The acknowledgment has no epoch. Raw fields remain available for invalid tests.
func (client *Client) SendLeaseBreakAcknowledgment(ctx context.Context, header wire.Header, acknowledgment wire.LeaseBreakRequest) error {
	body, err := wire.EncodeLeaseBreakRequest(acknowledgment)
	if err != nil {
		return err
	}
	header.Command = wire.OplockBreak
	return client.Send(ctx, []wire.Message{{Header: header, Body: body}})
}
