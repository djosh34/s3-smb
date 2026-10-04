package auth

import (
	"bytes"
	"encoding/hex"
	"errors"
	"io"
	"strconv"
	"testing"
	"time"
)

var vectorAccount = Account{User: "User", Domain: "Domain", Password: "Password"}

func hexBytes(t testing.TB, value string) []byte {
	t.Helper()
	data, err := hex.DecodeString(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func requireBytes(t testing.TB, actual, expected []byte) {
	t.Helper()
	if !bytes.Equal(actual, expected) {
		t.Fatalf("got %x, want %x", actual, expected)
	}
}

// MS-NLMP 4.2.4.1 and 4.2.4.2: NTLMv2 authentication with User, Domain,
// Password, server nonce 0123456789abcdef, client nonce aa and session key 55.
// https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-nlmp/
func TestMSNLMPVectors(t *testing.T) {
	key, err := responseKey(vectorAccount, "User", "Domain")
	if err != nil {
		t.Fatal(err)
	}
	requireBytes(t, key, hexBytes(t, "0c868a403bfd7a93a3001ef22ef02e3f"))
	info := hexBytes(t, "02000c0044006f006d00610069006e0001000c0053006500720076006500720000000000")
	blob := responseBlob(make([]byte, 8), bytes.Repeat([]byte{0xaa}, 8), info)
	requireBytes(t, blob, hexBytes(t, "01010000000000000000000000000000aaaaaaaaaaaaaaaa0000000002000c0044006f006d00610069006e0001000c005300650072007600650072000000000000000000"))
	proof, err := ntlmHMAC(key, hexBytes(t, "0123456789abcdef"), blob)
	if err != nil {
		t.Fatal(err)
	}
	requireBytes(t, proof, hexBytes(t, "68cd0ab851e51c96aabc927bebef6a1c"))
	baseKey, err := ntlmHMAC(key, proof)
	if err != nil {
		t.Fatal(err)
	}
	requireBytes(t, baseKey, hexBytes(t, "8de40ccadbc14a82f15cb0ad0de95ca3"))
	encrypted, err := exchangeKey(baseKey, bytes.Repeat([]byte{0x55}, 16))
	if err != nil {
		t.Fatal(err)
	}
	requireBytes(t, encrypted, hexBytes(t, "c5dad2544fc9799094ce1ce90bc9d03e"))
	decrypted, err := exchangeKey(baseKey, encrypted)
	if err != nil {
		t.Fatal(err)
	}
	requireBytes(t, decrypted, bytes.Repeat([]byte{0x55}, 16))
}

func testAcceptor(t testing.TB, account Account) *Acceptor {
	t.Helper()
	acceptor, err := NewAcceptor(Options{
		Account: account, ServerName: "Server",
		Random: bytes.NewReader(hexBytes(t, "0123456789abcdef")),
		Now:    func() time.Time { return time.Date(1601, 1, 1, 0, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return acceptor
}

func testInitiator(t testing.TB, account Account) *Initiator {
	t.Helper()
	random := append(bytes.Repeat([]byte{0xaa}, 8), bytes.Repeat([]byte{0x55}, 16)...)
	initiator, err := NewInitiator(account, bytes.NewReader(random))
	if err != nil {
		t.Fatal(err)
	}
	return initiator
}

func startExchange(t testing.TB, acceptor *Acceptor, initiator *Initiator) (Result, Result) {
	t.Helper()
	initial, err := acceptor.InitialToken()
	if err != nil {
		t.Fatal(err)
	}
	negotiate, err := initiator.Start(initial)
	if err != nil {
		t.Fatal(err)
	}
	if negotiate.Done || negotiate.User != "" || len(negotiate.SessionKey) != 0 {
		t.Fatal("negotiate exposed an authenticated result")
	}
	challenge, err := acceptor.Step(negotiate.Token)
	if err != nil {
		t.Fatal(err)
	}
	if challenge.Done || challenge.User != "" || len(challenge.SessionKey) != 0 {
		t.Fatal("challenge exposed an authenticated result")
	}
	authenticate, err := initiator.Step(challenge.Token)
	if err != nil {
		t.Fatal(err)
	}
	if authenticate.Done || authenticate.User != "" || len(authenticate.SessionKey) != 16 {
		t.Fatal("initiator key must arrive before final acceptance")
	}
	return challenge, authenticate
}

func TestVectorExchange(t *testing.T) {
	acceptor, initiator := testAcceptor(t, vectorAccount), testInitiator(t, vectorAccount)
	challenge, authenticate := startExchange(t, acceptor, initiator)
	wrapped, err := decodeSPNEGO(challenge.Token)
	if err != nil {
		t.Fatal(err)
	}
	message, err := decodeNTLM(wrapped.token)
	if err != nil {
		t.Fatal(err)
	}
	requireBytes(t, message.raw[24:32], hexBytes(t, "0123456789abcdef"))
	requireBytes(t, message.av[avTimestamp], make([]byte, 8))
	wrapped, err = decodeSPNEGO(authenticate.Token)
	if err != nil {
		t.Fatal(err)
	}
	message, err = decodeNTLM(wrapped.token)
	if err != nil {
		t.Fatal(err)
	}
	requireBytes(t, message.blob[16:24], bytes.Repeat([]byte{0xaa}, 8))
	// Independently calculated from the MS-NLMP inputs with the server's
	// timestamp and the client's MIC flag added to the transcript.
	requireBytes(t, message.fields[1][:16], hexBytes(t, "884fa22befc5628983a77abe3da7db53"))
	requireBytes(t, message.fields[5], hexBytes(t, "d0380d9477ece2c7d2e58f3473d21bfe"))
	requireBytes(t, message.raw[message.micOffset:message.micOffset+16], hexBytes(t, "6f15f888be24b42b587710b57ff028ac"))
	requireBytes(t, authenticate.SessionKey, bytes.Repeat([]byte{0x55}, 16))
	original := bytes.Clone(authenticate.Token)
	final, err := acceptor.Step(authenticate.Token)
	if err != nil {
		t.Fatal(err)
	}
	if !final.Done || final.User != "User" {
		t.Fatal("server did not return the canonical authenticated user")
	}
	requireBytes(t, final.SessionKey, authenticate.SessionKey)
	requireBytes(t, authenticate.Token, original)
	done, err := initiator.Step(final.Token)
	if err != nil || !done.Done || done.User != "User" {
		t.Fatalf("initiator final result %+v: %v", done, err)
	}
	if _, err := acceptor.Step(authenticate.Token); err == nil {
		t.Fatal("completed exchange accepted a replay")
	}
	if _, err := initiator.Step(final.Token); err == nil {
		t.Fatal("completed initiator accepted a replay")
	}
}

func TestIdentityPolicy(t *testing.T) {
	tests := []struct {
		name   string
		server Account
		client Account
		ok     bool
	}{
		{name: "case", server: vectorAccount, client: Account{User: "USER", Domain: "DOMAIN", Password: "Password"}, ok: true},
		{name: "local domain", server: Account{User: "User", Password: "Password"}, client: vectorAccount, ok: true},
		{name: "local empty domain", server: Account{User: "User", Password: "Password"}, client: Account{User: "User", Password: "Password"}, ok: true},
		{name: "wrong password", server: vectorAccount, client: Account{User: "User", Domain: "Domain", Password: "wrong"}},
		{name: "wrong user", server: vectorAccount, client: Account{User: "other", Domain: "Domain", Password: "Password"}},
		{name: "wrong domain", server: vectorAccount, client: Account{User: "User", Domain: "other", Password: "Password"}},
		{name: "guest", server: vectorAccount, client: Account{User: "Guest", Domain: "Domain", Password: "Password"}},
		{name: "unicode", server: Account{User: "üser😀", Password: "Password"}, client: Account{User: "ÜSER😀", Password: "Password"}, ok: true},
		{name: "empty password", server: Account{User: "User"}, client: Account{User: "User"}, ok: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			acceptor := testAcceptor(t, test.server)
			_, authenticate := startExchange(t, acceptor, testInitiator(t, test.client))
			result, err := acceptor.Step(authenticate.Token)
			if test.ok {
				if err != nil || !result.Done || result.User != test.server.User {
					t.Fatalf("login did not succeed: %v", err)
				}
			} else {
				requireFailure(t, result, err)
			}
		})
	}
}

func requireFailure(t testing.TB, result Result, err error) {
	t.Helper()
	if err == nil || result.Done || result.User != "" || len(result.SessionKey) != 0 || len(result.Token) != 0 {
		t.Fatalf("failed authentication exposed a result: %+v, %v", result, err)
	}
}

func TestBadProofAndMIC(t *testing.T) {
	for _, test := range []struct {
		mutate func(ntlmMessage)
		name   string
	}{
		{name: "proof", mutate: func(message ntlmMessage) { message.fields[1][0] ^= 1 }},
		{name: "MIC", mutate: func(message ntlmMessage) { message.raw[message.micOffset] ^= 1 }},
		{name: "MIC removed", mutate: func(message ntlmMessage) { clear(message.raw[message.micOffset : message.micOffset+16]) }},
		{name: "MIC flag cleared", mutate: func(message ntlmMessage) { clear(message.av[avFlags]) }},
		{name: "server name", mutate: func(message ntlmMessage) { message.av[avComputer][0] ^= 1 }},
		{name: "negotiated flags", mutate: func(message ntlmMessage) { littleEndian.PutUint32(message.raw[60:], message.flags^flagKeyExch) }},
		{name: "anonymous", mutate: func(message ntlmMessage) { littleEndian.PutUint32(message.raw[60:], message.flags|flagAnonymous) }},
		{name: "session key", mutate: func(message ntlmMessage) { message.fields[5][0] ^= 1 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			acceptor := testAcceptor(t, vectorAccount)
			_, authenticate := startExchange(t, acceptor, testInitiator(t, vectorAccount))
			wrapped, err := decodeSPNEGO(authenticate.Token)
			if err != nil {
				t.Fatal(err)
			}
			message, err := decodeNTLM(wrapped.token)
			if err != nil {
				t.Fatal(err)
			}
			test.mutate(message)
			token, err := encodeResponse(-1, nil, message.raw, nil)
			if err != nil {
				t.Fatal(err)
			}
			result, err := acceptor.Step(token)
			requireFailure(t, result, err)
			result, err = acceptor.Step(authenticate.Token)
			requireFailure(t, result, err)
		})
	}
}

type failingReader struct{ err error }

func (reader failingReader) Read([]byte) (int, error) { return 0, reader.err }

func TestRandomErrors(t *testing.T) {
	failure := errors.New("random source failed")
	acceptor := testAcceptor(t, vectorAccount)
	initial, err := acceptor.InitialToken()
	if err != nil {
		t.Fatal(err)
	}
	initiator := testInitiator(t, vectorAccount)
	negotiate, err := initiator.Start(initial)
	if err != nil {
		t.Fatal(err)
	}
	acceptor.options.Random = failingReader{err: failure}
	result, err := acceptor.Step(negotiate.Token)
	requireFailure(t, result, err)
	if !errors.Is(err, failure) {
		t.Fatalf("lost random error: %v", err)
	}
	for _, length := range []int{0, 4, 8, 12, 23} {
		t.Run(strconv.Itoa(length), func(t *testing.T) {
			acceptor := testAcceptor(t, vectorAccount)
			initiator := testInitiator(t, vectorAccount)
			initial, err := acceptor.InitialToken()
			if err != nil {
				t.Fatal(err)
			}
			negotiate, err := initiator.Start(initial)
			if err != nil {
				t.Fatal(err)
			}
			challenge, err := acceptor.Step(negotiate.Token)
			if err != nil {
				t.Fatal(err)
			}
			initiator.random = bytes.NewReader(make([]byte, length))
			result, err := initiator.Step(challenge.Token)
			requireFailure(t, result, err)
			if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("lost short random source error: %v", err)
			}
		})
	}
}

func TestConfiguration(t *testing.T) {
	for _, account := range []Account{
		{},
		{User: "bad\x00user"},
		{User: string([]byte{0xff})},
		{User: "user", Domain: "bad\x00domain"},
		{User: "user", Password: string([]byte{0xff})},
		{User: string(bytes.Repeat([]byte{'x'}, 1025))},
	} {
		if _, err := NewInitiator(account, nil); err == nil {
			t.Fatal("invalid account accepted by initiator")
		}
		if _, err := NewAcceptor(Options{Account: account, ServerName: "Server"}); err == nil {
			t.Fatal("invalid account accepted by acceptor")
		}
	}
	for _, name := range []string{"", "bad\x00name", string([]byte{0xff})} {
		if _, err := NewAcceptor(Options{Account: vectorAccount, ServerName: name}); err == nil {
			t.Fatal("invalid server name accepted")
		}
	}
	acceptor, err := NewAcceptor(Options{Account: vectorAccount, ServerName: "Server"})
	if err != nil || acceptor.options.Random == nil || acceptor.options.Now == nil {
		t.Fatalf("default nondeterminism missing: %v", err)
	}
	initiator, err := NewInitiator(vectorAccount, nil)
	if err != nil || initiator.random == nil {
		t.Fatalf("default random source missing: %v", err)
	}
}

func TestTimestamp(t *testing.T) {
	for _, test := range []struct {
		time time.Time
		hex  string
	}{
		{time: time.Date(1601, 1, 1, 0, 0, 0, 0, time.UTC), hex: "0000000000000000"},
		{time: time.Unix(0, 0), hex: "00803ed5deb19d01"},
		{time: time.Unix(0, 123456789), hex: "875651d5deb19d01"},
	} {
		stamp, err := ntlmTimestamp(test.time)
		if err != nil {
			t.Fatal(err)
		}
		requireBytes(t, stamp, hexBytes(t, test.hex))
	}
	for _, year := range []int{1600, 10000} {
		acceptor := testAcceptor(t, vectorAccount)
		acceptor.options.Now = func() time.Time { return time.Date(year, 1, 1, 0, 0, 0, 0, time.UTC) }
		token, err := encodeInitial(negotiateMessage())
		if err != nil {
			t.Fatal(err)
		}
		result, err := acceptor.Step(token)
		requireFailure(t, result, err)
	}
}
