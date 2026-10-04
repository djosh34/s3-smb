// Package auth owns NTLMv2 inside SPNEGO for one configured user. It does not
// own SMB sessions, SMB signing keys, tree access or connection I/O. NTLMv1,
// guest, anonymous and Kerberos authentication are not supported.
package auth

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"encoding/asn1"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// Account is the single allowed identity. An empty Domain selects a local user:
// a client domain contributes to the proof but does not select another account.
// A nonempty Domain must match the client domain, ignoring case. User matching
// also ignores case. Passwords and derived secrets must never appear in logs.
type Account struct {
	User     string
	Domain   string
	Password string
}

// Options supplies identity and nondeterminism for reproducible MS-NLMP vectors.
// Random defaults to crypto/rand.Reader and must be secure in production.
// Now defaults to time.Now. ServerName is the NTLM target, not the share name.
type Options struct {
	Random     io.Reader
	Now        func() time.Time
	Account    Account
	ServerName string
}

// Result is one handshake step. Acceptor returns the canonical User and exported
// SessionKey only after verifying the proof and any supplied MICs. Initiator
// supplies its key when emitting Authenticate, before Done, so the raw client
// can verify the final SESSION_SETUP signature. crypt derives SMB keys from this
// key. Failed steps return a zero Result and an error, never an authenticated
// result.
type Result struct {
	User       string
	Token      []byte
	SessionKey []byte
	Done       bool
}

const (
	exchangeNew = iota + 1
	exchangeChallenge
	exchangeFinal
	exchangeNegotiate

	// FILETIME starts in 1601, 11644473600 seconds before the Unix epoch.
	filetimeUnixOffsetSeconds int64 = 11644473600
	// Last second of year 9999, measured from the FILETIME epoch. This is
	// our clock's supported calendar range, not the maximum uint64 FILETIME.
	lastSupportedFiletimeSecond int64 = 265046774399
)

var (
	errExchange  = errors.New("authentication exchange is not in the expected state")
	errLogin     = errors.New("authentication failed")
	errMechanism = errors.New("only NTLMSSP inside SPNEGO is supported")
)

// Acceptor handles one server-side exchange. It is not shared by sessions or
// used concurrently. Use NewAcceptor; a failed Step consumes the exchange.
type Acceptor struct {
	options        Options
	negotiate      []byte
	mechList       []byte
	challenge      ntlmMessage
	state          int
	requireMechMIC bool
}

// NewAcceptor validates the account and creates an unused authentication exchange.
func NewAcceptor(options Options) (*Acceptor, error) {
	if err := validateAccount(options.Account); err != nil {
		return nil, err
	}
	if err := validateText(options.ServerName); err != nil {
		return nil, err
	}
	if options.ServerName == "" {
		return nil, errors.New("NTLM server name is empty")
	}
	if options.Random == nil {
		options.Random = rand.Reader
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	return &Acceptor{options: options, state: exchangeNew}, nil
}

func validateAccount(account Account) error {
	if account.User == "" {
		return errors.New("authentication user is empty")
	}
	for _, value := range []string{account.User, account.Domain, account.Password} {
		if err := validateText(value); err != nil {
			return err
		}
	}
	return nil
}

// InitialToken advertises only NTLMSSP and does not start an exchange.
func (acceptor *Acceptor) InitialToken() ([]byte, error) {
	if acceptor == nil || acceptor.state == 0 {
		return nil, errExchange
	}
	return encodeInitial(nil)
}

// Step selects NTLM from the client's SPNEGO offer, then consumes NTLM
// Negotiate and Authenticate. A malformed token or bad credentials terminate
// this exchange. Selecting a later mechanism requires an extra negotiate step
// and a mechanism-list MIC, as RFC 4178 requires.
func (acceptor *Acceptor) Step(token []byte) (Result, error) {
	if acceptor == nil {
		return Result{}, errExchange
	}
	state := acceptor.state
	acceptor.state = 0
	wrapped, err := decodeSPNEGO(token)
	if err != nil {
		return Result{}, err
	}
	switch state {
	case exchangeNew:
		return acceptor.start(wrapped)
	case exchangeChallenge:
		return acceptor.finish(wrapped)
	case exchangeNegotiate:
		if !validClientResponse(wrapped) || len(wrapped.mic) != 0 {
			return Result{}, errToken
		}
		return acceptor.startNTLM(wrapped.token, false)
	default:
		return Result{}, errExchange
	}
}

func (acceptor *Acceptor) start(wrapped spnegoToken) (Result, error) {
	if !wrapped.initial {
		return Result{}, errMechanism
	}
	for index, mechanism := range wrapped.mechs {
		if !mechanism.Equal(ntlmOID) {
			continue
		}
		acceptor.mechList = bytes.Clone(wrapped.mechList)
		acceptor.requireMechMIC = index != 0
		if index == 0 && len(wrapped.token) != 0 {
			return acceptor.startNTLM(wrapped.token, true)
		}
		// RFC 4178 3.2(c)(II): discard another mechanism's optimistic token
		// and request a MIC when selecting something other than first choice.
		state := 1
		if acceptor.requireMechMIC {
			state = 3
		}
		token, err := encodeResponse(state, ntlmOID, nil, nil)
		if err != nil {
			return Result{}, err
		}
		acceptor.state = exchangeNegotiate
		return Result{Token: token}, nil
	}
	return Result{}, errMechanism
}

func validClientResponse(wrapped spnegoToken) bool {
	return !wrapped.initial && wrapped.state != 2 && wrapped.state != 3 &&
		(len(wrapped.mechanism) == 0 || wrapped.mechanism.Equal(ntlmOID))
}

func (acceptor *Acceptor) startNTLM(raw []byte, firstReply bool) (Result, error) {
	negotiate, err := decodeNTLM(raw)
	if err != nil {
		return Result{}, err
	}
	if negotiate.kind != messageNegotiate || negotiate.flags&requiredFlags != requiredFlags || negotiate.flags&flagAnonymous != 0 {
		return Result{}, errLogin
	}
	flags := negotiate.flags&(offeredFlags|flagSeal) | flagTargetInfo | flagTarget | flagServer
	if flags&(flagSign|flagSeal) == 0 {
		flags &^= flagKeyExch
	}
	challenge, err := acceptor.makeChallenge(flags)
	if err != nil {
		return Result{}, err
	}
	decoded, err := decodeNTLM(challenge)
	if err != nil {
		return Result{}, err
	}
	var mechanism asn1.ObjectIdentifier
	if firstReply {
		mechanism = ntlmOID
	}
	token, err := encodeResponse(1, mechanism, challenge, nil)
	if err != nil {
		return Result{}, err
	}
	acceptor.negotiate = bytes.Clone(raw)
	acceptor.challenge = decoded
	acceptor.state = exchangeChallenge
	return Result{Token: token}, nil
}

func (acceptor *Acceptor) makeChallenge(flags uint32) ([]byte, error) {
	stamp, err := ntlmTimestamp(acceptor.options.Now())
	if err != nil {
		return nil, err
	}
	domain := acceptor.options.Account.Domain
	if domain == "" {
		domain = acceptor.options.ServerName
	}
	pairs := []avPair{
		{id: avComputer, value: encodeUTF16(acceptor.options.ServerName)},
		{id: avDomain, value: encodeUTF16(domain)},
		{id: avTimestamp, value: stamp},
	}
	info, err := encodeAV(pairs)
	if err != nil {
		return nil, err
	}
	target := encodeUTF16(acceptor.options.ServerName)
	floor := 48
	if flags&flagVersion != 0 {
		floor += 8
	}
	data := newNTLM(messageChallenge, floor+len(target)+len(info))
	littleEndian.PutUint32(data[20:], flags)
	if _, randomErr := io.ReadFull(acceptor.options.Random, data[24:32]); randomErr != nil {
		return nil, fmt.Errorf("read NTLM server challenge: %w", randomErr)
	}
	if flags&flagVersion != 0 {
		copy(data[48:56], ntlmVersion)
	}
	start, err := putField(data, 12, floor, target)
	if err != nil {
		return nil, err
	}
	if _, fieldErr := putField(data, 40, start, info); fieldErr != nil {
		return nil, fieldErr
	}
	return data, nil
}

func ntlmTimestamp(now time.Time) ([]byte, error) {
	seconds := now.Unix() + filetimeUnixOffsetSeconds
	if seconds < 0 || seconds > lastSupportedFiletimeSecond {
		return nil, errors.New("NTLM clock is outside supported years 1601 through 9999")
	}
	stamp := make([]byte, 8)
	ticks := uint64(seconds)*10000000 + uint64(now.Nanosecond()/100) //nolint:gosec // time.Time.Nanosecond is in [0, 999999999].
	littleEndian.PutUint64(stamp, ticks)
	return stamp, nil
}

func (acceptor *Acceptor) finish(wrapped spnegoToken) (Result, error) {
	if !validClientResponse(wrapped) || (acceptor.requireMechMIC && len(wrapped.mic) == 0) {
		return Result{}, errMechanism
	}
	authenticate, err := decodeNTLM(wrapped.token)
	if err != nil {
		return Result{}, err
	}
	if authenticate.kind != messageAuthenticate {
		return Result{}, errToken
	}
	key, err := acceptor.verify(authenticate)
	if err != nil {
		return Result{}, err
	}
	var serverMIC []byte
	if len(wrapped.mic) != 0 {
		flags := authenticate.flags & acceptor.challenge.flags
		clientMIC, micErr := mechanismMIC(key, acceptor.mechList, flags, true)
		if micErr != nil {
			return Result{}, micErr
		}
		if !hmac.Equal(clientMIC, wrapped.mic) {
			return Result{}, errLogin
		}
		serverMIC, err = mechanismMIC(key, acceptor.mechList, flags, false)
		if err != nil {
			return Result{}, err
		}
	}
	token, err := encodeResponse(0, nil, nil, serverMIC)
	if err != nil {
		return Result{}, err
	}
	return Result{Token: token, User: acceptor.options.Account.User, SessionKey: key, Done: true}, nil
}

func (acceptor *Acceptor) verify(message ntlmMessage) ([]byte, error) {
	if message.flags&requiredFlags != requiredFlags || message.flags&flagAnonymous != 0 {
		return nil, errLogin
	}
	if message.flags&flagKeyExch != 0 && (acceptor.challenge.flags&flagKeyExch == 0 || message.flags&(flagSign|flagSeal) == 0) {
		return nil, errLogin
	}
	user, err := decodeUTF16(message.fields[3])
	if err != nil {
		return nil, err
	}
	domain, err := decodeUTF16(message.fields[2])
	if err != nil {
		return nil, err
	}
	account := acceptor.options.Account
	if !strings.EqualFold(user, account.User) || (account.Domain != "" && !strings.EqualFold(domain, account.Domain)) {
		return nil, errLogin
	}
	return acceptor.verifyProof(message, user, domain)
}

func (acceptor *Acceptor) verifyProof(message ntlmMessage, user, domain string) ([]byte, error) {
	responseKey, err := responseKey(acceptor.options.Account, user, domain)
	if err != nil {
		return nil, err
	}
	proof, err := ntlmHMAC(responseKey, acceptor.challenge.raw[24:32], message.blob)
	if err != nil {
		return nil, err
	}
	if !hmac.Equal(proof, message.fields[1][:16]) {
		return nil, errLogin
	}
	baseKey, err := ntlmHMAC(responseKey, proof)
	if err != nil {
		return nil, err
	}
	key := baseKey
	if message.flags&flagKeyExch != 0 {
		key, err = exchangeKey(baseKey, message.fields[5])
		if err != nil {
			return nil, err
		}
	}
	if message.hasMIC() {
		mic, err := transcriptMIC(key, acceptor.negotiate, acceptor.challenge.raw, message.raw, message.micOffset)
		if err != nil {
			return nil, err
		}
		if !hmac.Equal(mic, message.raw[message.micOffset:message.micOffset+16]) {
			return nil, errLogin
		}
	}
	return key, nil
}

// Initiator supplies the minimal NTLMv2 exchange for the raw test client, not a
// general SMB client. It is not used concurrently. Use NewInitiator.
type Initiator struct {
	random    io.Reader
	account   Account
	negotiate []byte
	state     int
}

// NewInitiator validates the account. A nil random source uses crypto/rand.Reader.
func NewInitiator(account Account, random io.Reader) (*Initiator, error) {
	if err := validateAccount(account); err != nil {
		return nil, err
	}
	if random == nil {
		random = rand.Reader
	}
	return &Initiator{account: account, random: random, state: exchangeNew}, nil
}

// Start consumes the server's mechanism list and emits a negotiate token.
func (initiator *Initiator) Start(serverToken []byte) (Result, error) {
	if initiator == nil || initiator.state != exchangeNew {
		return Result{}, errExchange
	}
	initiator.state = 0
	wrapped, err := decodeSPNEGO(serverToken)
	if err != nil {
		return Result{}, err
	}
	if !wrapped.initial || len(wrapped.mechs) != 1 || !wrapped.mechs[0].Equal(ntlmOID) || len(wrapped.token) != 0 || len(wrapped.mic) != 0 {
		return Result{}, errMechanism
	}
	negotiate := negotiateMessage()
	token, err := encodeInitial(negotiate)
	if err != nil {
		return Result{}, err
	}
	initiator.negotiate = negotiate
	initiator.state = exchangeChallenge
	return Result{Token: token}, nil
}

// Step emits Authenticate and its key, or validates the final SPNEGO acceptance.
// The SMB client must verify the final SESSION_SETUP signature separately.
func (initiator *Initiator) Step(serverToken []byte) (Result, error) {
	if initiator == nil {
		return Result{}, errExchange
	}
	state := initiator.state
	initiator.state = 0
	wrapped, err := decodeSPNEGO(serverToken)
	if err != nil {
		return Result{}, err
	}
	if wrapped.initial || len(wrapped.mic) != 0 {
		return Result{}, errToken
	}
	switch state {
	case exchangeChallenge:
		if wrapped.state != 1 || (len(wrapped.mechanism) != 0 && !wrapped.mechanism.Equal(ntlmOID)) {
			return Result{}, errMechanism
		}
		return initiator.authenticate(wrapped.token)
	case exchangeFinal:
		if wrapped.state != 0 || len(wrapped.token) != 0 || (len(wrapped.mechanism) != 0 && !wrapped.mechanism.Equal(ntlmOID)) {
			return Result{}, errLogin
		}
		return Result{User: initiator.account.User, Done: true}, nil
	default:
		return Result{}, errExchange
	}
}

func (initiator *Initiator) authenticate(token []byte) (Result, error) {
	challenge, err := decodeNTLM(token)
	if err != nil {
		return Result{}, err
	}
	if challenge.kind != messageChallenge || challenge.flags&requiredFlags != requiredFlags || challenge.flags&flagTargetInfo == 0 || len(challenge.av[avTimestamp]) != 8 || len(challenge.av[avComputer]) == 0 || len(challenge.av[avDomain]) == 0 {
		return Result{}, errLogin
	}
	var nonce [8]byte
	if _, randomErr := io.ReadFull(initiator.random, nonce[:]); randomErr != nil {
		return Result{}, fmt.Errorf("read NTLM client challenge: %w", randomErr)
	}
	info, err := targetInfoWithMIC(challenge.fields[1])
	if err != nil {
		return Result{}, err
	}
	blob := responseBlob(challenge.av[avTimestamp], nonce[:], info)
	responseKey, err := responseKey(initiator.account, initiator.account.User, initiator.account.Domain)
	if err != nil {
		return Result{}, err
	}
	proof, err := ntlmHMAC(responseKey, token[24:32], blob)
	if err != nil {
		return Result{}, err
	}
	baseKey, err := ntlmHMAC(responseKey, proof)
	if err != nil {
		return Result{}, err
	}
	flags := challenge.flags & offeredFlags
	key, encryptedKey, err := initiator.exportKey(baseKey, flags)
	if err != nil {
		return Result{}, err
	}
	authenticate, err := initiator.makeAuthenticate(flags, append(proof, blob...), encryptedKey)
	if err != nil {
		return Result{}, err
	}
	micOffset := 64
	if flags&flagVersion != 0 {
		micOffset += 8
	}
	mic, err := transcriptMIC(key, initiator.negotiate, token, authenticate, micOffset)
	if err != nil {
		return Result{}, err
	}
	copy(authenticate[micOffset:], mic)
	wrapped, err := encodeResponse(-1, nil, authenticate, nil)
	if err != nil {
		return Result{}, err
	}
	initiator.state = exchangeFinal
	return Result{Token: wrapped, SessionKey: key}, nil
}

func (initiator *Initiator) exportKey(baseKey []byte, flags uint32) ([]byte, []byte, error) {
	if flags&flagKeyExch == 0 {
		return baseKey, nil, nil
	}
	key := make([]byte, 16)
	if _, err := io.ReadFull(initiator.random, key); err != nil {
		return nil, nil, fmt.Errorf("read NTLM exported session key: %w", err)
	}
	encrypted, err := exchangeKey(baseKey, key)
	if err != nil {
		return nil, nil, err
	}
	return key, encrypted, nil
}

func (initiator *Initiator) makeAuthenticate(flags uint32, response, encryptedKey []byte) ([]byte, error) {
	floor := 64 + 16
	if flags&flagVersion != 0 {
		floor += 8
	}
	domain, user := encodeUTF16(initiator.account.Domain), encodeUTF16(initiator.account.User)
	size := floor + 24 + len(response) + len(domain) + len(user) + len(encryptedKey)
	if size > maxTokenSize {
		return nil, errToken
	}
	data := newNTLM(messageAuthenticate, size)
	littleEndian.PutUint32(data[60:], flags)
	if flags&flagVersion != 0 {
		copy(data[64:72], ntlmVersion)
	}
	start := floor
	for _, payload := range []struct {
		value  []byte
		offset int
	}{
		{offset: 12, value: make([]byte, 24)},
		{offset: 20, value: response},
		{offset: 28, value: domain},
		{offset: 36, value: user},
		{offset: 52, value: encryptedKey},
	} {
		var err error
		start, err = putField(data, payload.offset, start, payload.value)
		if err != nil {
			return nil, err
		}
	}
	return data, nil
}
