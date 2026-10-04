package server

import (
	"bytes"
	"encoding/binary"
	"errors"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

var errAccessDenied = errors.New("request protection refused")

// savedProtection retains keys for replies after a session is removed. Entries
// live through the final reply, including an async final after LOGOFF.
type savedProtection struct {
	session   *sessionEntry
	encrypted bool
}

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
		if err := verifyPlaintextRequest(message, session); err != nil {
			return messages, nil, err
		}
	}
	return messages, encrypted, nil
}

func verifyPlaintextRequest(message wire.Message, session *sessionEntry) error {
	header := message.Header
	if header.Command == wire.Cancel && header.Flags&wire.FlagSigned != 0 && (session == nil || !session.active || session.protector == nil) {
		return errAccessDenied
	}
	if session == nil || session.protector == nil {
		return nil
	}
	if session.identity.Encrypted && header.Command != wire.SessionSetup {
		return errAccessDenied
	}
	// CANCEL does not require a signature (MS-SMB2 3.3.5.16). A signed
	// CANCEL still needs a valid session and signature; refusal sends no reply.
	if header.Command == wire.Cancel && header.Flags&wire.FlagSigned == 0 {
		return nil
	}
	if err := session.protector.Verify(message.Raw); err != nil {
		return errAccessDenied
	}
	return nil
}

func (connection *connection) rememberProtection(messages []wire.Message, encrypted *sessionEntry) {
	connection.sessionMu.Lock()
	defer connection.sessionMu.Unlock()
	var preceding uint64
	for _, message := range messages {
		id := message.Header.SessionID
		if message.Header.Flags&wire.FlagRelated != 0 {
			id = preceding
		}
		preceding = id
		session := connection.sessions[id]
		if session != nil && message.Header.Command != wire.Cancel {
			connection.replyProtection[message.Header.MessageID] = savedProtection{session: session, encrypted: encrypted != nil}
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
			session := connection.sessions[message.Header.SessionID]
			if session == nil || session.preauth == nil {
				return nil, errors.New("SESSION_SETUP reply has no preauth session")
			}
			session.preauth.Update(payload)
		}
		if message.Header.Status != smb.StatusPending {
			delete(connection.replyProtection, message.Header.MessageID)
		}
	}
	return payload, nil
}

// responseProtection and signPayload run with sessionMu held by encodePayload.
func (connection *connection) responseProtection(messages []wire.Message) (*sessionEntry, error) {
	var encrypted *sessionEntry
	for index := range messages {
		message := &messages[index]
		saved := connection.replyProtection[message.Header.MessageID]
		session := saved.session
		if session == nil {
			session = connection.sessions[message.Header.SessionID]
		}
		var chosen *sessionEntry
		if session != nil && (saved.encrypted || session.identity.Encrypted && message.Header.Command != wire.SessionSetup) {
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
			session := connection.replyProtection[member.Header.MessageID].session
			if session == nil {
				session = connection.sessions[member.Header.SessionID]
			}
			if session == nil || session.protector == nil {
				return errors.New("signed reply has no session key")
			}
			signature, signErr := session.protector.Sign(member.Raw)
			if signErr != nil {
				return signErr
			}
			copy(payload[offset+48:offset+64], signature[:])
		}
		offset += len(member.Raw)
	}
	return nil
}
