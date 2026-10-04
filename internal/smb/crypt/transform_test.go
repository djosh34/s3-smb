package crypt

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"sync"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
)

// MS-SMB2 3.1.4.2 and 3.1.4.3 vectors calculated independently with Python
// HMAC-SHA256 and OpenSSL 3 EVP GCM. Inputs are the published Microsoft session
// key and preauth hash, testMember(true, false), and nonce 010203040100000000000000.
// GCM uses transform bytes 20..51 as AAD and stores its tag at bytes 4..19.
var transformVectors = []struct {
	sendKey    string
	receiveKey string
	transform  string
	cipherID   uint16
}{
	{
		cipherID:   smb.CipherAES128GCM,
		sendKey:    "E2AF0DCEFAC68DA71A0DFBD0D1350D74",
		receiveKey: "629BCBC54422A0F572B97F45989B6073",
		transform: `
		fd534d42969e31600ae9155e7607efbc7f556a900102030401000000000000000000000050000000000001001900000000100000
		66c9c344eadd13ace23b9ea4e69a8e6fe9532f287314125c367854134f5b23b1e4b85ae9a3c57bc28e7fd6fcfa2218370d2b639e5f19e2d505f496f478540d1f68d13fdab43b34f54456b0783a022f20`,
	},
	{
		cipherID:   smb.CipherAES256GCM,
		sendKey:    "35952fed051bf2471af5f7acc63674b887a54b47af46959f7caff1ce537d7419",
		receiveKey: "049c27fd8a340262e32c643dea2ba507af7a4c085fd2505aeffcbdeae6d6d8ab",
		transform: `
		fd534d42260fed62eb933a651a698a5b23756ac70102030401000000000000000000000050000000000001001900000000100000
		fbe82445570cc3fa8dfd47b87fac10024f084132606dcce42c6024eadec70237093e7ffa5314c3b110155efdeca868b7713eb3098ae0c06dd857942bcb74c75cb5fb769ac16a0fa0f1fdf7a237014e70`,
	},
}

func TestTransformVectors(t *testing.T) {
	for _, vector := range transformVectors {
		t.Run(fmt.Sprint(vector.cipherID), func(t *testing.T) {
			server := newTestProtector(t, smb.SigningGMAC, vector.cipherID, RoleServer)
			client := newTestProtector(t, smb.SigningGMAC, vector.cipherID, RoleClient)
			keySize := 16
			if vector.cipherID == smb.CipherAES256GCM {
				keySize = 32
			}
			for _, key := range []struct {
				label string
				want  string
			}{
				{label: "SMBS2CCipherKey", want: vector.sendKey},
				{label: "SMBC2SCipherKey", want: vector.receiveKey},
			} {
				got, err := deriveKey(testOptions(t, smb.SigningGMAC, vector.cipherID, RoleServer).SessionKey, key.label, vectorContext(t), keySize)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(got, decodeHex(t, key.want)) {
					t.Fatalf("%s = %x, want %s", key.label, got, key.want)
				}
			}
			plaintext := testMember(true, false)
			got, err := server.Seal(plaintext)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, decodeHex(t, vector.transform)) {
				t.Fatalf("transform = %x, want %s", got, vector.transform)
			}
			opened, err := client.Open(decodeHex(t, vector.transform))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(opened, plaintext) {
				t.Fatal("vector decrypted incorrectly")
			}
		})
	}
}

func TestGCMProtection(t *testing.T) {
	for _, cipherID := range []uint16{smb.CipherAES128GCM, smb.CipherAES256GCM} {
		t.Run(fmt.Sprint(cipherID), func(t *testing.T) {
			server := newTestProtector(t, smb.SigningCMAC, cipherID, RoleServer)
			client := newTestProtector(t, smb.SigningCMAC, cipherID, RoleClient)
			for _, pair := range [][2]*Protector{{server, client}, {client, server}} {
				plain := testMember(false, false)
				plain[16] &^= flagSigned
				original := bytes.Clone(plain)
				transform, err := pair[0].Seal(plain)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(plain, original) {
					t.Fatal("Seal changed or separately signed plaintext")
				}
				saved := bytes.Clone(transform)
				opened, err := pair[1].Open(transform)
				if err != nil || !bytes.Equal(opened, original) {
					t.Fatalf("round trip: %x, %v", opened, err)
				}
				if !bytes.Equal(transform, saved) {
					t.Fatal("Open changed the input")
				}
				opened[0] ^= 1
				if !bytes.Equal(transform, saved) {
					t.Fatal("Open returned an alias of the input")
				}
				if opened, err := pair[0].Open(transform); err == nil || opened != nil {
					t.Fatal("wrong-direction key accepted encrypted input")
				}
				for i := range transform {
					changed := bytes.Clone(transform)
					changed[i] ^= 1
					if opened, err := pair[1].Open(changed); err == nil || opened != nil {
						t.Errorf("exposed plaintext after transform byte %d changed", i)
					}
				}
				for size := 0; size < len(transform); size++ {
					if opened, err := pair[1].Open(transform[:size]); err == nil || opened != nil {
						t.Errorf("accepted truncated transform of size %d", size)
					}
				}
				if opened, err := pair[1].Open(append(bytes.Clone(transform), 0)); err == nil || opened != nil {
					t.Fatal("accepted trailing bytes")
				}
			}
			for size := 0; size < smbHeaderSize; size++ {
				if data, err := server.Seal(make([]byte, size)); err == nil || data != nil {
					t.Errorf("sealed short payload of size %d", size)
				}
			}
		})
	}
}

func TestConcurrentNonces(t *testing.T) {
	for _, cipherID := range []uint16{smb.CipherAES128GCM, smb.CipherAES256GCM} {
		t.Run(fmt.Sprint(cipherID), func(t *testing.T) {
			server := newTestProtector(t, smb.SigningGMAC, cipherID, RoleServer)
			client := newTestProtector(t, smb.SigningGMAC, cipherID, RoleClient)
			const count = 512
			nonces := make(chan [12]byte, count)
			plain := testMember(true, false)
			var workers sync.WaitGroup
			for range count {
				workers.Go(func() {
					sealed, err := server.Seal(plain)
					if err != nil {
						t.Error(err)
						return
					}
					opened, err := client.Open(sealed)
					if err != nil || !bytes.Equal(opened, plain) {
						t.Errorf("concurrent round trip: %v", err)
					}
					var nonce [12]byte
					copy(nonce[:], sealed[20:32])
					nonces <- nonce
				})
			}
			workers.Wait()
			close(nonces)
			seen := make(map[[12]byte]bool, count)
			for nonce := range nonces {
				if seen[nonce] {
					t.Errorf("reused nonce %x", nonce)
				}
				seen[nonce] = true
			}
			if len(seen) != count {
				t.Fatalf("got %d nonces, want %d", len(seen), count)
			}
		})
	}
}

func TestReconnectKeys(t *testing.T) {
	server := newTestProtector(t, smb.SigningGMAC, smb.CipherAES256GCM, RoleServer)
	options := testOptions(t, smb.SigningGMAC, smb.CipherAES256GCM, RoleClient)
	options.Preauth[0] ^= 1
	reconnected, err := NewProtector(options)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := server.Seal(testMember(true, false))
	if err != nil {
		t.Fatal(err)
	}
	if opened, err := reconnected.Open(sealed); err == nil || opened != nil {
		t.Fatal("fresh reconnect keys accepted old encrypted traffic")
	}
	member := testMember(true, false)
	tag, err := server.Sign(member)
	if err != nil {
		t.Fatal(err)
	}
	copy(member[48:64], tag[:])
	if err := reconnected.Verify(member); err == nil {
		t.Fatal("fresh reconnect key accepted old signature")
	}
	if reconnected.counter != 0 {
		t.Fatal("new protector did not start a fresh nonce counter")
	}
}

func TestTransformSizeValidation(t *testing.T) {
	server := newTestProtector(t, smb.SigningGMAC, smb.CipherAES128GCM, RoleServer)
	client := newTestProtector(t, smb.SigningGMAC, smb.CipherAES128GCM, RoleClient)
	sealed, err := server.Seal(testMember(true, false))
	if err != nil {
		t.Fatal(err)
	}
	binary.LittleEndian.PutUint32(sealed[36:40], 0xffffffff)
	if opened, err := client.Open(sealed); err == nil || opened != nil {
		t.Fatal("accepted an oversized OriginalMessageSize")
	}
}

func FuzzOpenTransform(f *testing.F) {
	for _, vector := range transformVectors {
		f.Add(decodeHex(f, vector.transform), vector.cipherID)
	}
	f.Add([]byte{}, uint16(smb.CipherAES128GCM))
	f.Add(make([]byte, transformHeaderSize), uint16(smb.CipherAES256GCM))
	f.Fuzz(func(t *testing.T, input []byte, cipherID uint16) {
		if cipherID != smb.CipherAES128GCM && cipherID != smb.CipherAES256GCM {
			return
		}
		protector := newTestProtector(t, smb.SigningGMAC, cipherID, RoleClient)
		before := bytes.Clone(input)
		plain, err := protector.Open(input)
		if err != nil && plain != nil {
			t.Fatal("failed authentication exposed plaintext")
		}
		if !bytes.Equal(input, before) {
			t.Fatal("Open changed the input")
		}
		if err == nil && uint64(len(plain)) != uint64(binary.LittleEndian.Uint32(input[36:40])) {
			t.Fatal("authenticated size differs from plaintext")
		}
	})
}
