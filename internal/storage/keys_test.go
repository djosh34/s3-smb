// SPDX-License-Identifier: AGPL-3.0-only
package storage

import (
	"bytes"
	"encoding/asn1"
	"encoding/pem"
	"sync"
	"testing"

	"github.com/emmansun/gmsm/pkcs"
)

const testPassphrase = "synthetic-test-passphrase"

var (
	fixtureOnce sync.Once
	fixtureKey  []byte
	fixtureErr  error
)

// protectedFixture generates one key per test binary because scrypt is slow.
func protectedFixture(t *testing.T) []byte {
	t.Helper()
	fixtureOnce.Do(func() {
		fixtureKey, fixtureErr = generateKey(testPassphrase)
	})
	if fixtureErr != nil {
		t.Fatal(fixtureErr)
	}
	return bytes.Clone(fixtureKey)
}

func marshalASN1(t *testing.T, v any) []byte {
	t.Helper()
	data, err := asn1.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestProtectedKeyProfile(t *testing.T) {
	data := protectedFixture(t)
	key, err := unlockKey(data, testPassphrase)
	if err != nil {
		t.Fatal(err)
	}
	if key.N.BitLen() != 3072 {
		t.Fatal("wrong key size")
	}
	if _, err = unlockKey(data, "wrong"); err == nil {
		t.Fatal("accepted wrong passphrase")
	}
	for name, bad := range map[string][]byte{
		"oversized": bytes.Repeat([]byte{'x'}, maxKeyBytes+1),
		"empty":     nil,
		"extra":     append(bytes.Clone(data), data...),
		"plaintext": pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("bad")}),
	} {
		if _, err = unlockKey(bad, testPassphrase); err == nil {
			t.Errorf("accepted %s key", name)
		}
	}

	block, _ := pem.Decode(data)
	var envelope encryptedKeyInfo
	if err = unmarshalExact(block.Bytes, &envelope); err != nil {
		t.Fatal(err)
	}
	tampered := envelope
	tampered.Data = bytes.Clone(envelope.Data)
	tampered.Data[len(tampered.Data)-1] ^= 1
	if _, err = unlockKey(pem.EncodeToMemory(&pem.Block{Type: block.Type, Bytes: marshalASN1(t, tampered)}), testPassphrase); err == nil {
		t.Fatal("accepted unauthenticated ciphertext")
	}

	// A stored key must not choose its own scrypt cost.
	var params pkcs.PBES2Params
	if err = unmarshalExact(envelope.Algorithm.Parameters.FullBytes, &params); err != nil {
		t.Fatal(err)
	}
	var kdf scryptParameters
	if err = unmarshalExact(params.KeyDerivationFunc.Parameters.FullBytes, &kdf); err != nil {
		t.Fatal(err)
	}
	for _, n := range []int{1 << 30, 0, -1, 32768} {
		kdf.N = n
		params.KeyDerivationFunc.Parameters = asn1.RawValue{FullBytes: marshalASN1(t, kdf)}
		envelope.Algorithm.Parameters = asn1.RawValue{FullBytes: marshalASN1(t, params)}
		bad := pem.EncodeToMemory(&pem.Block{Type: block.Type, Bytes: marshalASN1(t, envelope)})
		if err = validateKeyEnvelope(bad); err == nil {
			t.Fatalf("accepted unapproved cost %d", n)
		}
	}
}
