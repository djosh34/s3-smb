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
	transform string
	cipherID  uint16
}{
	{
		cipherID: smb.CipherAES128GCM,
		transform: `
		fd534d42969e31600ae9155e7607efbc7f556a900102030401000000000000000000000050000000000001001900000000100000
		66c9c344eadd13ace23b9ea4e69a8e6fe9532f287314125c367854134f5b23b1e4b85ae9a3c57bc28e7fd6fcfa2218370d2b639e5f19e2d505f496f478540d1f68d13fdab43b34f54456b0783a022f20`,
	},
	{
		cipherID: smb.CipherAES256GCM,
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
				transform := checkGCMRoundTrip(t, pair[0], pair[1])
				checkGCMCorruption(t, pair[1], transform)
			}
			for size := 0; size < smbHeaderSize; size++ {
				if data, err := server.Seal(make([]byte, size)); err == nil || data != nil {
					t.Errorf("sealed short payload of size %d", size)
				}
			}
		})
	}
}

func checkGCMRoundTrip(t *testing.T, sender, receiver *Protector) []byte {
	t.Helper()
	plain := testMember(false, false)
	plain[16] &^= flagSigned
	original := bytes.Clone(plain)
	transform, err := sender.Seal(plain)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(plain, original) {
		t.Fatal("Seal changed or separately signed plaintext")
	}
	saved := bytes.Clone(transform)
	opened, err := receiver.Open(transform)
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
	if wrongDirection, openErr := sender.Open(transform); openErr == nil || wrongDirection != nil {
		t.Fatal("wrong-direction key accepted encrypted input")
	}
	return transform
}

func checkGCMCorruption(t *testing.T, receiver *Protector, transform []byte) {
	t.Helper()
	for i := range transform {
		changed := bytes.Clone(transform)
		changed[i] ^= 1
		if opened, err := receiver.Open(changed); err == nil || opened != nil {
			t.Errorf("exposed plaintext after transform byte %d changed", i)
		}
	}
	for size := 0; size < len(transform); size++ {
		if opened, err := receiver.Open(transform[:size]); err == nil || opened != nil {
			t.Errorf("accepted truncated transform of size %d", size)
		}
	}
	if opened, err := receiver.Open(append(bytes.Clone(transform), 0)); err == nil || opened != nil {
		t.Fatal("accepted trailing bytes")
	}
}

func TestConcurrentNonces(t *testing.T) {
	server := newTestProtector(t, smb.SigningGMAC, smb.CipherAES128GCM, RoleServer)
	const count = 64
	nonces := make(chan [12]byte, count)
	var workers sync.WaitGroup
	for range count {
		workers.Go(func() {
			sealed, err := server.Seal(testMember(true, false))
			if err != nil {
				t.Error(err)
				return
			}
			nonces <- [12]byte(sealed[20:32])
		})
	}
	workers.Wait()
	close(nonces)
	seen := make(map[[12]byte]bool, count)
	for nonce := range nonces {
		if seen[nonce] {
			t.Fatalf("reused nonce %x", nonce)
		}
		seen[nonce] = true
	}
}

// A peer can send nonzero Reserved bytes. They stay part of the authenticated
// header but do not change the transform's meaning (MS-SMB2 2.2.41).
func TestTransformReservedBytes(t *testing.T) {
	server := newTestProtector(t, smb.SigningGMAC, smb.CipherAES128GCM, RoleServer)
	client := newTestProtector(t, smb.SigningGMAC, smb.CipherAES128GCM, RoleClient)
	plain := testMember(false, false)
	transform, err := client.Seal(plain)
	if err != nil {
		t.Fatal(err)
	}
	transform[40], transform[41] = 0x12, 0x34
	sealed := client.send.Seal(nil, transform[20:32], plain, transform[20:52])
	copy(transform[4:20], sealed[len(plain):])
	copy(transform[52:], sealed[:len(plain)])
	opened, err := server.Open(transform)
	if err != nil || !bytes.Equal(opened, plain) {
		t.Fatalf("authenticated reserved bytes were rejected: %v", err)
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
