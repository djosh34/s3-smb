package server

import (
	"crypto/rand"
	"fmt"
	"slices"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/auth"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

type algorithms struct {
	cipher            uint16
	signing           uint16
	encryptionOffered bool
	signingOffered    bool
}

func (connection *connection) refuse(status smb.Status, reason string) reply {
	connection.server.options.Logger.Info("negotiate refused", "reason", reason)
	return reply{status: status}
}

func (connection *connection) sendWildcard() error {
	body, err := connection.negotiateResponse(smb.DialectWildcard, nil, nil)
	if err != nil {
		return err
	}
	request := wire.Header{Command: wire.Negotiate, Credit: 1}
	if err := connection.credits.consume([]wire.Message{{Header: request}}); err != nil {
		return err
	}
	return connection.send([]wire.Message{{Header: wire.Header{Command: wire.Negotiate, Flags: wire.FlagResponse, Credit: connection.credits.grant(request)}, Body: body}})
}

func (connection *connection) negotiate(message wire.Message) (reply, error) {
	request, err := wire.DecodeNegotiateRequest(message)
	if err != nil {
		return reply{}, fmt.Errorf("decode validated NEGOTIATE: %w", err)
	}
	if !slices.Contains(request.Dialects, smb.Dialect311) {
		return connection.refuse(smb.StatusNotSupported, "client must offer SMB 3.1.1"), nil
	}
	selected, status, reason := selectAlgorithms(request.Contexts, connection.server.options.Encryption)
	if status != smb.StatusSuccess {
		return connection.refuse(status, reason), nil
	}
	contexts, err := selected.contexts()
	if err != nil {
		return reply{}, err
	}
	options := connection.server.options
	acceptor, err := auth.NewAcceptor(auth.Options{Account: options.Account, ServerName: options.ServerName, Now: options.Now})
	if err != nil {
		return reply{}, err
	}
	token, err := acceptor.InitialToken()
	if err != nil {
		return reply{}, err
	}
	body, err := connection.negotiateResponse(smb.Dialect311, contexts, token)
	if err != nil {
		return reply{}, err
	}
	connection.negotiated = true
	connection.clientGUID = request.ClientGUID
	connection.cipher, connection.signing = selected.cipher, selected.signing
	connection.preauth.Update(message.Raw)
	return reply{body: body}, nil
}

func (connection *connection) negotiateResponse(dialect uint16, contexts []wire.NegotiateContext, token []byte) ([]byte, error) {
	options := connection.server.options
	now, err := wire.EncodeFiletime(options.Now())
	if err != nil {
		return nil, fmt.Errorf("negotiate clock: %w", err)
	}
	return wire.EncodeNegotiateResponse(wire.NegotiateResponse{
		Token: token, Contexts: contexts, Dialect: dialect,
		SecurityMode: smb.AdvertisedSecurityMode, ServerGUID: options.ServerGUID, Capabilities: smb.AdvertisedCapabilities,
		MaxTransact: smb.MaxTransactSize, MaxRead: smb.MaxReadSize, MaxWrite: smb.MaxWriteSize, SystemTime: uint64(now),
	})
}

func selectAlgorithms(contexts []wire.NegotiateContext, policy EncryptionPolicy) (algorithms, smb.Status, string) {
	selected := algorithms{signing: smb.SigningCMAC}
	seen := make(map[uint16]bool)
	for _, context := range contexts {
		if uniqueNegotiateContext(context.Type) {
			if seen[context.Type] {
				return algorithms{}, smb.StatusInvalidParameter, "duplicate negotiate context"
			}
			seen[context.Type] = true
		}
		switch context.Type {
		case wire.ContextPreauth:
			preauth, err := wire.DecodePreauthContext(context)
			if err != nil {
				return algorithms{}, smb.StatusInvalidParameter, "malformed preauth context"
			}
			if !slices.Contains(preauth.Hashes, smb.PreauthSHA512) {
				return algorithms{}, smb.StatusSMBNoPreauthIntegrityHashOverlap, "client must offer SHA-512 preauth integrity"
			}
		case wire.ContextEncryption:
			encryption, err := wire.DecodeEncryptionContext(context)
			if err != nil {
				return algorithms{}, smb.StatusInvalidParameter, "malformed encryption context"
			}
			selected.encryptionOffered = true
			selected.cipher = prefer(encryption.Ciphers, smb.CipherAES256GCM, smb.CipherAES128GCM)
		case wire.ContextSigning:
			signing, err := wire.DecodeSigningContext(context)
			if err != nil {
				return algorithms{}, smb.StatusInvalidParameter, "malformed signing context"
			}
			selected.signingOffered = true
			selected.signing = prefer(signing.Algorithms, smb.SigningGMAC, smb.SigningCMAC)
			if selected.signing == 0 {
				selected.signing = smb.SigningCMAC
			}
		}
	}
	if !seen[wire.ContextPreauth] {
		return algorithms{}, smb.StatusInvalidParameter, "SMB 3.1.1 requires one preauth context"
	}
	if policy == RequireEncryption && selected.cipher == 0 {
		return algorithms{}, smb.StatusNotSupported, "client offers no AES-GCM; turn encryption off or update macOS"
	}
	return selected, smb.StatusSuccess, ""
}

func uniqueNegotiateContext(contextType uint16) bool {
	switch contextType {
	// MS-SMB2 3.3.5.4 also forbids duplicate compression (3) and RDMA (7) contexts.
	case wire.ContextPreauth, wire.ContextEncryption, wire.ContextSigning, 3, 7:
		return true
	default:
		return false
	}
}

func prefer(offered []uint16, first, second uint16) uint16 {
	if slices.Contains(offered, first) {
		return first
	}
	if slices.Contains(offered, second) {
		return second
	}
	return 0
}

func (selected algorithms) contexts() ([]wire.NegotiateContext, error) {
	salt := make([]byte, 32)
	if _, err := rand.Read(salt); err != nil {
		return nil, fmt.Errorf("preauth salt: %w", err)
	}
	preauth, err := wire.EncodePreauthContext(wire.PreauthContext{Hashes: []uint16{smb.PreauthSHA512}, Salt: salt})
	if err != nil {
		return nil, err
	}
	contexts := []wire.NegotiateContext{preauth}
	if selected.encryptionOffered {
		// Cipher zero reports no common cipher; it never enables CCM.
		encryption, err := wire.EncodeEncryptionContext(wire.EncryptionContext{Ciphers: []uint16{selected.cipher}})
		if err != nil {
			return nil, err
		}
		contexts = append(contexts, encryption)
	}
	if selected.signingOffered {
		signing, err := wire.EncodeSigningContext(wire.SigningContext{Algorithms: []uint16{selected.signing}})
		if err != nil {
			return nil, err
		}
		contexts = append(contexts, signing)
	}
	return contexts, nil
}
