package smbtest

import (
	"bytes"
	"errors"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func (client *Client) encodeMessages(messages []wire.Message) ([]byte, error) {
	client.protectionMu.RLock()
	defer client.protectionMu.RUnlock()
	if client.protector == nil {
		return wire.Join(messages)
	}
	messages = append([]wire.Message(nil), messages...)
	for index := range messages {
		messages[index].Header.Signature = [16]byte{}
		messages[index].Header.Flags &^= wire.FlagSigned
		if !client.encrypted {
			messages[index].Header.Flags |= wire.FlagSigned
		}
	}
	payload, err := wire.Join(messages)
	if err != nil {
		return nil, err
	}
	if client.encrypted {
		return client.protector.Seal(payload)
	}
	members, err := wire.Split(payload)
	if err != nil {
		return nil, err
	}
	offset := 0
	for _, member := range members {
		signature, err := client.protector.Sign(member.Raw)
		if err != nil {
			return nil, err
		}
		copy(payload[offset+48:offset+64], signature[:])
		offset += len(member.Raw)
	}
	return payload, nil
}

func (client *Client) decodeMessages(payload []byte) (Reply, error) {
	client.protectionMu.RLock()
	defer client.protectionMu.RUnlock()
	reply := Reply{Raw: payload}
	encrypted := bytes.HasPrefix(payload, []byte{0xfd, 'S', 'M', 'B'})
	if encrypted {
		if client.protector == nil {
			return reply, errors.New("smbtest: transform before login")
		}
		var err error
		payload, err = client.protector.Open(payload)
		if err != nil {
			return reply, err
		}
	}
	messages, err := wire.Split(payload)
	reply.Messages = messages
	if err != nil {
		return reply, err
	}
	if client.protector == nil {
		return reply, nil
	}
	for _, message := range messages {
		if message.Header.Flags&wire.FlagResponse == 0 || message.Header.SessionID != client.sessionID && !isLeaseBreak(message.Header) {
			return reply, errors.New("smbtest: protected reply identity mismatch")
		}
		if !encrypted {
			if err := client.verifyPlaintextReply(message); err != nil {
				return reply, err
			}
		}
	}
	return reply, nil
}

// Called with protectionMu held, after checking the reply identity.
func (client *Client) verifyPlaintextReply(message wire.Message) error {
	if client.encrypted && message.Header.Command != wire.SessionSetup {
		return errors.New("smbtest: encrypted session received plaintext")
	}
	if message.Header.Status == smb.StatusPending && message.Header.Flags&wire.FlagSigned == 0 {
		return nil
	}
	// MS-SMB2 3.3.4.7 permits unsigned plaintext lease notifications.
	// Validate the notification body before bypassing verification.
	if !client.encrypted && isLeaseBreak(message.Header) && message.Header.Flags&wire.FlagSigned == 0 {
		if len(message.Body) != 44 {
			return errors.New("smbtest: invalid unsigned lease break length")
		}
		_, err := wire.DecodeLeaseBreakNotification(message)
		return err
	}
	return client.protector.Verify(message.Raw)
}
