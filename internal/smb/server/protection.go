package server

import (
	"bytes"
	"encoding/binary"
	"errors"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func (connection *connection) decodePayload(payload []byte) ([]wire.Message, *sessionEntry, error) {
	var encrypted *sessionEntry
	if bytes.HasPrefix(payload, []byte{0xfd, 'S', 'M', 'B'}) {
		if len(payload) < 52 {
			return nil, nil, errors.New("short encryption transform")
		}
		id := binary.LittleEndian.Uint64(payload[44:52])
		connection.sessionMu.RLock()
		encrypted = connection.sessions[id]
		var err error
		if encrypted == nil || encrypted.protector == nil {
			err = errors.New("transform has no authenticated session")
		} else {
			payload, err = encrypted.protector.Open(payload)
		}
		connection.sessionMu.RUnlock()
		if err != nil {
			return nil, nil, err
		}
	}
	messages, err := wire.Split(payload)
	if err != nil {
		return nil, nil, err
	}
	// Verify the entire chain before dispatch or credit consumption. Related
	// placeholders affect identity lookup, not the bytes covered by signatures.
	connection.sessionMu.RLock()
	defer connection.sessionMu.RUnlock()
	var preceding uint64
	for _, message := range messages {
		header := message.Header
		id := header.SessionID
		if header.Flags&wire.FlagRelated != 0 {
			id = preceding
		}
		preceding = id
		session := connection.sessions[id]
		if encrypted != nil {
			if id != encrypted.identity.SessionID || header.Command == wire.Negotiate || header.Command == wire.SessionSetup {
				return nil, nil, errors.New("encrypted compound has an invalid session or command")
			}
			continue
		}
		if session == nil || session.protector == nil {
			continue
		}
		if session.identity.Encrypted && header.Command != wire.SessionSetup {
			return nil, nil, errors.New("encrypted session received plaintext")
		}
		if err := session.protector.Verify(message.Raw); err != nil {
			return nil, nil, err
		}
	}
	return messages, encrypted, nil
}

func (connection *connection) rememberEncryption(messages []wire.Message, session *sessionEntry) {
	if session == nil {
		return
	}
	connection.sessionMu.Lock()
	defer connection.sessionMu.Unlock()
	for _, message := range messages {
		if message.Header.Command != wire.Cancel {
			connection.encryptedReplies[message.Header.MessageID] = session
		}
	}
}

func (connection *connection) encodePayload(messages []wire.Message) ([]byte, error) {
	connection.sessionMu.Lock()
	defer connection.sessionMu.Unlock()
	encrypted, err := connection.responseProtection(messages)
	if err != nil {
		return nil, err
	}
	payload, err := wire.Join(messages)
	if err != nil {
		return nil, err
	}
	if encrypted != nil {
		payload, err = encrypted.protector.Seal(payload)
	} else {
		err = connection.signPayload(payload)
	}
	if err != nil {
		return nil, err
	}
	for _, message := range messages {
		if message.Header.Command == wire.Negotiate && message.Header.Status == smb.StatusSuccess && connection.negotiated {
			connection.preauth.Update(payload)
		}
		if message.Header.Command == wire.SessionSetup && message.Header.Status == smb.StatusMoreProcessingRequired {
			connection.sessions[message.Header.SessionID].preauth.Update(payload)
		}
		if message.Header.Status != smb.StatusPending {
			delete(connection.encryptedReplies, message.Header.MessageID)
		}
	}
	return payload, nil
}

// responseProtection and signPayload run with sessionMu held by encodePayload.
func (connection *connection) responseProtection(messages []wire.Message) (*sessionEntry, error) {
	var encrypted *sessionEntry
	for index := range messages {
		message := &messages[index]
		session := connection.sessions[message.Header.SessionID]
		chosen := connection.encryptedReplies[message.Header.MessageID]
		if session != nil && session.identity.Encrypted && message.Header.Command != wire.SessionSetup {
			chosen = session
		}
		if chosen != nil {
			if encrypted != nil && encrypted != chosen {
				return nil, errors.New("response compound spans encrypted sessions")
			}
			encrypted = chosen
		}
		message.Header.Signature = [16]byte{}
		message.Header.Flags &^= wire.FlagSigned
		if chosen == nil && session != nil && session.protector != nil && message.Header.Status != smb.StatusPending {
			message.Header.Flags |= wire.FlagSigned
		}
	}
	if encrypted != nil {
		for index := range messages {
			if messages[index].Header.SessionID != encrypted.identity.SessionID {
				return nil, errors.New("response compound crosses an encrypted session")
			}
			messages[index].Header.Flags &^= wire.FlagSigned
		}
	}
	return encrypted, nil
}

func (connection *connection) signPayload(payload []byte) error {
	members, err := wire.Split(payload)
	if err != nil {
		return err
	}
	offset := 0
	for _, member := range members {
		if member.Header.Flags&wire.FlagSigned != 0 {
			signature, signErr := connection.sessions[member.Header.SessionID].protector.Sign(member.Raw)
			if signErr != nil {
				return signErr
			}
			copy(payload[offset+48:offset+64], signature[:])
		}
		offset += len(member.Raw)
	}
	return nil
}
