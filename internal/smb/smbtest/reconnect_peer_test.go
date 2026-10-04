package smbtest_test

import (
	"errors"
	"net"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/auth"
	"github.com/djosh34/s3-smb/internal/smb/crypt"
	"github.com/djosh34/s3-smb/internal/smb/smbtest"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

const (
	reconnectSessionID = 99
	reconnectTreeID    = 21
)

func peerEncode(message wire.Message, protector *crypt.Protector, encrypted bool) ([]byte, error) {
	if protector != nil && !encrypted {
		message.Header.Flags |= wire.FlagSigned
	}
	payload, err := wire.Join([]wire.Message{message})
	if err != nil || protector == nil {
		return payload, err
	}
	if encrypted {
		return protector.Seal(payload)
	}
	signature, err := protector.Sign(payload)
	if err != nil {
		return nil, err
	}
	copy(payload[48:64], signature[:])
	return payload, nil
}

func peerReceive(peer net.Conn, protector *crypt.Protector, encrypted bool) (wire.Message, []byte, error) {
	payload, err := readFrame(peer)
	if err != nil {
		return wire.Message{}, nil, err
	}
	if protector != nil {
		if encrypted {
			payload, err = protector.Open(payload)
		} else {
			err = protector.Verify(payload)
		}
		if err != nil {
			return wire.Message{}, nil, err
		}
	}
	messages, err := wire.Split(payload)
	if err != nil {
		return wire.Message{}, nil, err
	}
	if len(messages) != 1 {
		return wire.Message{}, nil, errors.New("expected one request")
	}
	return messages[0], payload, nil
}

func scriptReconnectLogin(peer net.Conn, previous smbtest.Session, account auth.Account, requireEncryption bool) (*crypt.Protector, error) {
	acceptor, err := auth.NewAcceptor(auth.Options{Account: account, ServerName: "server"})
	if err != nil {
		return nil, err
	}
	initial, err := acceptor.InitialToken()
	if err != nil {
		return nil, err
	}
	message, raw, err := peerReceive(peer, nil, false)
	if err != nil {
		return nil, err
	}
	request, err := wire.DecodeNegotiateRequest(message)
	if err != nil {
		return nil, err
	}
	if request.ClientGUID != previous.ClientGUID || message.Header.MessageID != 0 {
		return nil, errors.New("reconnect client GUID or message ID changed")
	}
	if checkErr := checkReconnectAlgorithms(request.Contexts, previous); checkErr != nil {
		return nil, checkErr
	}
	preauth := crypt.NewPreauth()
	preauth.Update(raw)
	body, err := wire.EncodeNegotiateResponse(wire.NegotiateResponse{Dialect: smb.Dialect311, SecurityMode: smb.AdvertisedSecurityMode, Token: initial, Contexts: request.Contexts})
	if err != nil {
		return nil, err
	}
	header := wire.Header{Command: wire.Negotiate, Flags: wire.FlagResponse, Credit: 5}
	payload, err := peerEncode(wire.Message{Header: header, Body: body}, nil, false)
	if err != nil {
		return nil, err
	}
	preauth.Update(payload)
	if writeErr := writePayload(peer, payload); writeErr != nil {
		return nil, writeErr
	}
	protector, err := scriptReconnectSetup(peer, previous, acceptor, preauth, requireEncryption)
	if err != nil {
		return nil, err
	}
	message, _, err = peerReceive(peer, protector, previous.Cipher != 0)
	if err != nil {
		return nil, err
	}
	tree, err := wire.DecodeTreeConnectRequest(message)
	if err != nil {
		return nil, err
	}
	if tree.Path != "\\\\server\\backup" || message.Header.MessageID != 3 || message.Header.SessionID != reconnectSessionID {
		return nil, errors.New("reconnect TREE_CONNECT changed")
	}
	body, err = wire.EncodeTreeConnectResponse(wire.TreeConnectResponse{ShareType: 1})
	if err != nil {
		return nil, err
	}
	header = wire.Header{Command: wire.TreeConnect, Flags: wire.FlagResponse, MessageID: 3, SessionID: reconnectSessionID, TreeID: reconnectTreeID, Credit: 5}
	payload, err = peerEncode(wire.Message{Header: header, Body: body}, protector, previous.Cipher != 0)
	if err != nil {
		return nil, err
	}
	if err := writePayload(peer, payload); err != nil {
		return nil, err
	}
	return protector, nil
}

func checkReconnectAlgorithms(contexts []wire.NegotiateContext, previous smbtest.Session) error {
	var signing, cipher bool
	for _, context := range contexts {
		switch context.Type {
		case wire.ContextPreauth:
		case wire.ContextSigning:
			selected, err := wire.DecodeSigningContext(context)
			if err != nil {
				return err
			}
			if len(selected.Algorithms) != 1 || selected.Algorithms[0] != previous.Signing {
				return errors.New("reconnect signing offer changed")
			}
			signing = true
		case wire.ContextEncryption:
			selected, err := wire.DecodeEncryptionContext(context)
			if err != nil {
				return err
			}
			if len(selected.Ciphers) != 1 || selected.Ciphers[0] != previous.Cipher {
				return errors.New("reconnect cipher offer changed")
			}
			cipher = true
		default:
			return errors.New("unexpected reconnect negotiate context")
		}
	}
	if !signing || cipher != (previous.Cipher != 0) {
		return errors.New("reconnect algorithms missing")
	}
	return nil
}

func scriptReconnectSetup(peer net.Conn, previous smbtest.Session, acceptor *auth.Acceptor, preauth *crypt.Preauth, requireEncryption bool) (*crypt.Protector, error) {
	var protector *crypt.Protector
	for id := uint64(1); id <= 2; id++ {
		message, raw, err := peerReceive(peer, nil, false)
		if err != nil {
			return nil, err
		}
		setup, err := wire.DecodeSessionSetupRequest(message)
		if err != nil {
			return nil, err
		}
		if setup.PreviousSessionID != previous.SessionID || message.Header.MessageID != id || id == 2 && message.Header.SessionID != reconnectSessionID {
			return nil, errors.New("PreviousSessionID or SESSION_SETUP identity changed")
		}
		preauth.Update(raw)
		result, err := acceptor.Step(setup.Token)
		if err != nil {
			return nil, err
		}
		status := smb.StatusMoreProcessingRequired
		if result.Done {
			status = smb.StatusSuccess
			protector, err = crypt.NewProtector(crypt.Options{SessionKey: result.SessionKey, Preauth: preauth.Sum(), SessionID: reconnectSessionID, Cipher: previous.Cipher, Signing: previous.Signing, Role: crypt.RoleServer})
			if err != nil {
				return nil, err
			}
		}
		flags := uint16(0)
		if result.Done && requireEncryption {
			flags = smb.SessionEncryptData
		}
		body, err := wire.EncodeSessionSetupResponse(wire.SessionSetupResponse{Token: result.Token, Flags: flags})
		if err != nil {
			return nil, err
		}
		header := wire.Header{Command: wire.SessionSetup, Flags: wire.FlagResponse, MessageID: id, SessionID: reconnectSessionID, Status: status, Credit: 5}
		payload, err := peerEncode(wire.Message{Header: header, Body: body}, protector, false)
		if err != nil {
			return nil, err
		}
		if !result.Done {
			preauth.Update(payload)
		}
		if err := writePayload(peer, payload); err != nil {
			return nil, err
		}
	}
	if protector == nil {
		return nil, errors.New("scripted login did not finish")
	}
	return protector, nil
}
