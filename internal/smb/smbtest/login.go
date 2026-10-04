package smbtest

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/auth"
	"github.com/djosh34/s3-smb/internal/smb/crypt"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

// LoginOptions selects the handshake. Cipher zero requests signed plaintext;
// a GCM cipher requests encryption. Algorithms use smb constants. Tests send
// raw messages to offer unsupported values. Share is the single disk share.
type LoginOptions struct {
	Share             string
	Account           auth.Account
	ClientGUID        [16]byte
	PreviousSessionID uint64
	Cipher            uint16
	Signing           uint16
}

// Session is the handshake identity and remaining credit balance.
// NextMessageID is the first unused ID after login; Send never allocates IDs.
// Reconnect uses a new client with PreviousSessionID, then DH2C and RqLs.
type Session struct {
	SessionID     uint64
	NextMessageID uint64
	TreeID        uint32
	Credits       uint16
	Cipher        uint16
	Signing       uint16
}

// Login negotiates SMB 3.1.1, authenticates NTLMv2 and connects the share. It
// verifies the final setup signature before accepting the session. Call once,
// before any other I/O. Send and Receive then apply protection automatically.
// A failed login closes the client; it cannot reuse the partial exchange.
func (client *Client) Login(ctx context.Context, options LoginOptions) (Session, error) {
	session, err := client.login(ctx, options)
	if err != nil {
		return Session{}, errors.Join(err, client.Close())
	}
	return session, nil
}

func (client *Client) login(ctx context.Context, options LoginOptions) (Session, error) {
	client.protectionMu.RLock()
	already := client.protector != nil
	client.protectionMu.RUnlock()
	if already {
		return Session{}, errors.New("smbtest: already logged in")
	}
	initiator, err := auth.NewInitiator(options.Account, nil)
	if err != nil {
		return Session{}, err
	}
	preauth := crypt.NewPreauth()
	session := Session{Credits: 1}
	token, err := client.loginNegotiate(ctx, &session, options, preauth)
	if err != nil {
		return Session{}, err
	}
	if err := client.loginAuthenticate(ctx, &session, options, preauth, initiator, token); err != nil {
		return Session{}, err
	}
	if err := client.loginTree(ctx, &session, options.Share); err != nil {
		return Session{}, err
	}
	return session, nil
}

func (client *Client) loginNegotiate(ctx context.Context, session *Session, options LoginOptions, preauth *crypt.Preauth) ([]byte, error) {
	contexts, err := loginContexts(options)
	if err != nil {
		return nil, err
	}
	guid := options.ClientGUID
	if guid == [16]byte{} {
		if _, randomErr := rand.Read(guid[:]); randomErr != nil {
			return nil, randomErr
		}
	}
	body, err := wire.EncodeNegotiateRequest(wire.NegotiateRequest{Dialects: []uint16{smb.Dialect311}, Contexts: contexts, ClientGUID: guid, SecurityMode: smb.AdvertisedSecurityMode})
	if err != nil {
		return nil, err
	}
	negotiate, err := client.loginExchange(ctx, session, wire.Negotiate, body, preauth)
	if err != nil {
		return nil, err
	}
	if negotiate.Header.Status != smb.StatusSuccess {
		return nil, loginStatus(negotiate)
	}
	response, err := wire.DecodeNegotiateResponse(negotiate)
	if err != nil {
		return nil, err
	}
	if response.Dialect != smb.Dialect311 || response.SecurityMode&smb.AdvertisedSecurityMode != smb.AdvertisedSecurityMode {
		return nil, errors.New("smbtest: server did not select SMB 3.1.1 with signing")
	}
	session.Signing, session.Cipher, err = loginAlgorithms(response.Contexts, options)
	return response.Token, err
}

func (client *Client) loginAuthenticate(ctx context.Context, session *Session, options LoginOptions, preauth *crypt.Preauth, initiator *auth.Initiator, token []byte) error {
	result, err := initiator.Start(token)
	if err != nil {
		return err
	}
	for {
		reply, stepErr := client.loginSetup(ctx, session, options, preauth, result)
		if stepErr != nil {
			return stepErr
		}
		setup := reply.Messages[0]
		if setup.Header.Status != smb.StatusSuccess && setup.Header.Status != smb.StatusMoreProcessingRequired {
			return loginStatus(setup)
		}
		decoded, decodeErr := wire.DecodeSessionSetupResponse(setup)
		if decodeErr != nil {
			return decodeErr
		}
		result, err = initiator.Step(decoded.Token)
		if err != nil {
			return err
		}
		if setup.Header.Status == smb.StatusSuccess {
			if !result.Done || decoded.Flags&3 != 0 {
				return errors.New("smbtest: incomplete, guest or anonymous session")
			}
			client.protectionMu.Lock()
			client.encrypted = options.Cipher != 0 || decoded.Flags&4 != 0
			client.protectionMu.Unlock()
			return nil
		}
		preauth.Update(reply.Raw)
	}
}

func (client *Client) loginSetup(ctx context.Context, session *Session, options LoginOptions, preauth *crypt.Preauth, result auth.Result) (Reply, error) {
	body, err := wire.EncodeSessionSetupRequest(wire.SessionSetupRequest{Token: result.Token, SecurityMode: uint8(smb.AdvertisedSecurityMode), PreviousSessionID: options.PreviousSessionID})
	if err != nil {
		return Reply{}, err
	}
	message := wire.Message{Header: wire.Header{Command: wire.SessionSetup, MessageID: session.NextMessageID, SessionID: session.SessionID, CreditCharge: 1, Credit: 16}, Body: body}
	raw, err := wire.Join([]wire.Message{message})
	if err != nil {
		return Reply{}, err
	}
	preauth.Update(raw)
	if sendErr := client.Send(ctx, []wire.Message{message}); sendErr != nil {
		return Reply{}, sendErr
	}
	// Authenticate supplies the key before the server's final response.
	if len(result.SessionKey) != 0 {
		protector, protectErr := crypt.NewProtector(crypt.Options{SessionKey: result.SessionKey, Preauth: preauth.Sum(), SessionID: session.SessionID, Cipher: session.Cipher, Signing: session.Signing, Role: crypt.RoleClient})
		if protectErr != nil {
			return Reply{}, protectErr
		}
		client.protectionMu.Lock()
		client.protector = protector
		client.sessionID = session.SessionID
		client.protectionMu.Unlock()
	}
	reply, err := client.Receive(ctx)
	if err != nil {
		return Reply{}, err
	}
	if len(reply.Messages) != 1 {
		return Reply{}, errors.New("smbtest: compounded setup reply")
	}
	setup := reply.Messages[0]
	if setup.Header.Command != wire.SessionSetup || setup.Header.MessageID != session.NextMessageID || setup.Header.SessionID == 0 || session.SessionID != 0 && setup.Header.SessionID != session.SessionID {
		return Reply{}, errors.New("smbtest: setup reply identity mismatch")
	}
	session.SessionID = setup.Header.SessionID
	session.NextMessageID++
	session.Credits = session.Credits - 1 + setup.Header.Credit
	return reply, nil
}

func (client *Client) loginTree(ctx context.Context, session *Session, share string) error {
	host := "server"
	if address, _, err := net.SplitHostPort(client.conn.RemoteAddr().String()); err == nil {
		host = address
	}
	body, err := wire.EncodeTreeConnectRequest(wire.TreeConnectRequest{Path: "\\\\" + host + "\\" + share})
	if err != nil {
		return err
	}
	tree, err := client.loginExchange(ctx, session, wire.TreeConnect, body, nil)
	if err != nil {
		return err
	}
	if tree.Header.Status != smb.StatusSuccess {
		return loginStatus(tree)
	}
	if _, err := wire.DecodeTreeConnectResponse(tree); err != nil {
		return err
	}
	if tree.Header.TreeID == 0 {
		return errors.New("smbtest: tree ID is zero")
	}
	session.TreeID = tree.Header.TreeID
	return nil
}

func loginStatus(message wire.Message) error {
	return fmt.Errorf("smbtest: command %d failed with status %#x", message.Header.Command, message.Header.Status)
}

func (client *Client) loginExchange(ctx context.Context, session *Session, command wire.Command, body []byte, preauth *crypt.Preauth) (wire.Message, error) {
	message := wire.Message{Header: wire.Header{Command: command, MessageID: session.NextMessageID, SessionID: session.SessionID, CreditCharge: 1, Credit: 16}, Body: body}
	if preauth != nil {
		raw, err := wire.Join([]wire.Message{message})
		if err != nil {
			return wire.Message{}, err
		}
		preauth.Update(raw)
	}
	if err := client.Send(ctx, []wire.Message{message}); err != nil {
		return wire.Message{}, err
	}
	reply, err := client.Receive(ctx)
	if err != nil {
		return wire.Message{}, err
	}
	if len(reply.Messages) != 1 {
		return wire.Message{}, errors.New("smbtest: compounded handshake reply")
	}
	response := reply.Messages[0]
	if response.Header.Command != command || response.Header.MessageID != session.NextMessageID {
		return wire.Message{}, errors.New("smbtest: handshake reply identity mismatch")
	}
	session.NextMessageID++
	session.Credits = session.Credits - 1 + response.Header.Credit
	if preauth != nil {
		preauth.Update(reply.Raw)
	}
	return response, nil
}

func loginContexts(options LoginOptions) ([]wire.NegotiateContext, error) {
	if options.Signing != smb.SigningCMAC && options.Signing != smb.SigningGMAC {
		return nil, errors.New("smbtest: unsupported signing algorithm")
	}
	if options.Cipher != 0 && options.Cipher != smb.CipherAES128GCM && options.Cipher != smb.CipherAES256GCM {
		return nil, errors.New("smbtest: unsupported cipher")
	}
	salt := make([]byte, 32)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	preauth, err := wire.EncodePreauthContext(wire.PreauthContext{Hashes: []uint16{smb.PreauthSHA512}, Salt: salt})
	if err != nil {
		return nil, err
	}
	signing, err := wire.EncodeSigningContext(wire.SigningContext{Algorithms: []uint16{options.Signing}})
	if err != nil {
		return nil, err
	}
	contexts := []wire.NegotiateContext{preauth, signing}
	if options.Cipher != 0 {
		encryption, err := wire.EncodeEncryptionContext(wire.EncryptionContext{Ciphers: []uint16{options.Cipher}})
		if err != nil {
			return nil, err
		}
		contexts = append(contexts, encryption)
	}
	return contexts, nil
}

func loginAlgorithms(contexts []wire.NegotiateContext, options LoginOptions) (uint16, uint16, error) {
	var signing, cipher uint16
	var seenPreauth, seenSigning bool
	for _, context := range contexts {
		switch context.Type {
		case wire.ContextPreauth:
			preauth, err := wire.DecodePreauthContext(context)
			if err != nil {
				return 0, 0, err
			}
			if len(preauth.Hashes) != 1 || preauth.Hashes[0] != smb.PreauthSHA512 || seenPreauth {
				return 0, 0, errors.New("smbtest: invalid preauth selection")
			}
			seenPreauth = true
		case wire.ContextSigning:
			selected, err := wire.DecodeSigningContext(context)
			if err != nil {
				return 0, 0, err
			}
			if len(selected.Algorithms) != 1 || selected.Algorithms[0] != options.Signing || seenSigning {
				return 0, 0, errors.New("smbtest: invalid signing selection")
			}
			signing, seenSigning = selected.Algorithms[0], true
		case wire.ContextEncryption:
			selected, err := wire.DecodeEncryptionContext(context)
			if err != nil {
				return 0, 0, err
			}
			if len(selected.Ciphers) != 1 || selected.Ciphers[0] != options.Cipher || cipher != 0 {
				return 0, 0, errors.New("smbtest: invalid cipher selection")
			}
			cipher = selected.Ciphers[0]
		}
	}
	if !seenPreauth || !seenSigning || cipher != options.Cipher {
		return 0, 0, errors.New("smbtest: missing negotiated algorithm")
	}
	return signing, cipher, nil
}
