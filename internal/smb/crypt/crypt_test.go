package crypt

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"sync"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
)

const testSessionID = 0x100000000019

func testOptions(t *testing.T, signing, cipherID uint16, role Role) Options {
	t.Helper()
	return Options{
		SessionKey: decodeHex(t, "270E1BA896585EEB7AF3472D3B4C75A7"),
		Preauth:    vectorContext(t),
		SessionID:  testSessionID,
		Signing:    signing,
		Cipher:     cipherID,
		Role:       role,
		Random:     bytes.NewReader([]byte{1, 2, 3, 4}),
	}
}

func newTestProtector(t *testing.T, signing, cipherID uint16, role Role) *Protector {
	t.Helper()
	protector, err := NewProtector(testOptions(t, signing, cipherID, role))
	if err != nil {
		t.Fatal(err)
	}
	return protector
}

func testMember(response, cancel bool) []byte {
	member := make([]byte, 80)
	copy(member, []byte{0xfe, 'S', 'M', 'B'})
	binary.LittleEndian.PutUint16(member[4:6], 64)
	command := uint16(13)
	if cancel {
		command = commandCancel
	}
	binary.LittleEndian.PutUint16(member[12:14], command)
	flags := uint32(flagSigned)
	if response {
		flags |= flagResponse
	}
	binary.LittleEndian.PutUint32(member[16:20], flags)
	binary.LittleEndian.PutUint64(member[24:32], 0x0102030405060708)
	binary.LittleEndian.PutUint64(member[40:48], testSessionID)
	member[64] = 4
	return member
}

// These fixed GMAC vectors use MS-SMB2 3.1.4.1 nonce construction and the
// published Microsoft signing key. Tags were calculated independently with
// OpenSSL 3 EVP AES-128-GCM, with the entire zero-signature member as AAD.
func TestGMACVectorsConcurrent(t *testing.T) {
	protector := newTestProtector(t, smb.SigningGMAC, 0, RoleServer)
	vectors := []struct {
		tag      string
		response bool
		cancel   bool
	}{
		{tag: "2b2561b7023f3c134da38913f13aaff3"},
		{tag: "09d6ff32075d502dba17c245e691ec72", response: true},
		{tag: "d4bfb43bab8f8325667bf3235bd885cc", cancel: true},
	}
	var workers sync.WaitGroup
	for _, vector := range vectors {
		expected := decodeHex(t, vector.tag)
		for range 16 {
			workers.Go(func() {
				for range 20 {
					member := testMember(vector.response, vector.cancel)
					got, err := protector.Sign(member)
					if err != nil {
						t.Error(err)
						return
					}
					if !bytes.Equal(got[:], expected) {
						t.Errorf("GMAC = %x, want %x", got, expected)
						return
					}
					copy(member[48:64], got[:])
					if err := protector.Verify(member); err != nil {
						t.Error(err)
					}
				}
			})
		}
	}
	workers.Wait()
}

func TestSigningValidation(t *testing.T) {
	for _, signing := range []uint16{smb.SigningCMAC, smb.SigningGMAC} {
		t.Run(fmt.Sprint(signing), func(t *testing.T) {
			protector := newTestProtector(t, signing, 0, RoleServer)
			member := testMember(true, false)
			original := bytes.Clone(member)
			tag, err := protector.Sign(member)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(member, original) {
				t.Fatal("Sign changed the input")
			}
			copy(member[48:64], tag[:])
			signed := bytes.Clone(member)
			if err := protector.Verify(member); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(member, signed) {
				t.Fatal("Verify changed the input")
			}
			if _, err := protector.Sign(member); err == nil {
				t.Fatal("signed a nonzero signature field")
			}
			checkSigningCorruption(t, protector, member)
		})
	}
}

func checkSigningCorruption(t *testing.T, protector *Protector, member []byte) {
	t.Helper()
	for i := range member {
		changed := bytes.Clone(member)
		changed[i] ^= 1
		if err := protector.Verify(changed); err == nil {
			t.Errorf("accepted change at byte %d, including padding", i)
		}
	}
	for size := 0; size < smbHeaderSize; size++ {
		if _, err := protector.Sign(member[:size]); err == nil {
			t.Errorf("signed short member of size %d", size)
		}
		if err := protector.Verify(member[:size]); err == nil {
			t.Errorf("verified short member of size %d", size)
		}
	}
	malformed := [][]byte{testMember(true, false), testMember(true, false), testMember(true, false)}
	malformed[0][0] = 0xfd
	malformed[1][4] = 63
	malformed[2][16] &^= flagSigned
	for _, bad := range malformed {
		if _, err := protector.Sign(bad); err == nil {
			t.Fatal("signed a malformed header")
		}
	}
}

func TestSignedOnly(t *testing.T) {
	for _, signing := range []uint16{smb.SigningCMAC, smb.SigningGMAC} {
		protector := newTestProtector(t, signing, 0, RoleClient)
		member := testMember(false, false)
		tag, err := protector.Sign(member)
		if err != nil {
			t.Fatal(err)
		}
		copy(member[48:64], tag[:])
		if err := protector.Verify(member); err != nil {
			t.Fatal(err)
		}
		if data, err := protector.Seal(member); err == nil || data != nil {
			t.Fatal("signed-only protector encrypted data")
		}
		if data, err := protector.Open(make([]byte, 132)); err == nil || data != nil {
			t.Fatal("signed-only protector decrypted data")
		}
	}
}

func TestOptionsValidation(t *testing.T) {
	tests := []struct {
		change func(*Options)
		name   string
	}{
		{name: "empty key", change: func(o *Options) { o.SessionKey = nil }},
		{name: "short key", change: func(o *Options) { o.SessionKey = make([]byte, 15) }},
		{name: "long key", change: func(o *Options) { o.SessionKey = make([]byte, 32) }},
		{name: "role", change: func(o *Options) { o.Role = 2 }},
		{name: "no signing", change: func(o *Options) { o.Signing = 0 }},
		{name: "unknown signing", change: func(o *Options) { o.Signing = 99 }},
		{name: "CCM", change: func(o *Options) { o.Cipher = 1 }},
		{name: "unknown cipher", change: func(o *Options) { o.Cipher = 99 }},
		{name: "short random", change: func(o *Options) { o.Random = bytes.NewReader([]byte{1, 2, 3}) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			options := testOptions(t, smb.SigningCMAC, smb.CipherAES128GCM, RoleServer)
			test.change(&options)
			if protector, err := NewProtector(options); err == nil || protector != nil {
				t.Fatal("accepted invalid options")
			}
		})
	}
	options := testOptions(t, smb.SigningGMAC, smb.CipherAES256GCM, RoleServer)
	options.Random = nil
	if _, err := NewProtector(options); err != nil {
		t.Fatalf("default random reader: %v", err)
	}
	options.Random = bytes.NewReader(nil)
	if _, err := NewProtector(options); !errors.Is(err, io.EOF) {
		t.Fatalf("random reader error not preserved: %v", err)
	}
	options.Cipher = 0
	if _, err := NewProtector(options); err != nil {
		t.Fatalf("signed-only constructor read nonce material: %v", err)
	}
}

func TestNonceExhaustion(t *testing.T) {
	protector := newTestProtector(t, smb.SigningCMAC, smb.CipherAES128GCM, RoleServer)
	protector.counter = math.MaxUint64 - 1
	last, err := protector.Seal(testMember(true, false))
	if err != nil {
		t.Fatal(err)
	}
	if binary.LittleEndian.Uint64(last[24:32]) != math.MaxUint64 {
		t.Fatal("incorrect final nonce")
	}
	for range 2 {
		if data, err := protector.Seal(testMember(true, false)); err == nil || data != nil {
			t.Fatal("nonce counter wrapped")
		}
	}
}

func TestUninitializedProtector(t *testing.T) {
	var protector Protector
	member := testMember(true, false)
	if _, err := protector.Sign(member); err == nil {
		t.Fatal("uninitialized protector signed a message")
	}
	if err := protector.Verify(member); err == nil {
		t.Fatal("uninitialized protector verified a message")
	}
	if data, err := protector.Seal(member); err == nil || data != nil {
		t.Fatal("uninitialized protector encrypted a message")
	}
	if data, err := protector.Open(member); err == nil || data != nil {
		t.Fatal("uninitialized protector decrypted a message")
	}
}
