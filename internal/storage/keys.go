// SPDX-License-Identifier: AGPL-3.0-only
package storage

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/pem"
	"errors"

	"github.com/djosh34/s3-smb/internal/juicefs/pkg/object"
	"github.com/emmansun/gmsm/pkcs"
	"github.com/emmansun/gmsm/pkcs8"
)

const maxKeyBytes = 32768
const scryptN = 131072 // 128*N*r = 134217728 bytes; no attacker-selected costs.
var oidPBES2 = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 5, 13}
var oidScrypt = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 11591, 4, 11}
var oidAES256GCM = asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 1, 46}

// These are ASN.1 envelopes, not an alternative crypto implementation. Validate
// the untrusted cost/algorithm fields before calling the pinned native parser.
type encryptedKeyInfo struct {
	Algorithm pkix.AlgorithmIdentifier
	Data      []byte
}
type scryptParameters struct {
	Salt      []byte
	N, R, P   int
	KeyLength int `asn1:"optional"`
}
type gcmParameters struct {
	Nonce  []byte
	ICVLen int `asn1:"optional,default:12"`
}

func unmarshalExact(data []byte, v any) error {
	rest, err := asn1.Unmarshal(data, v)
	if err != nil || len(rest) != 0 {
		return errors.New("invalid protected key encoding")
	}
	return nil
}
func validateKeyEnvelope(data []byte) error {
	if len(data) == 0 || len(data) > maxKeyBytes {
		return errors.New("invalid protected key size")
	}
	block, rest := pem.Decode(data)
	if block == nil || block.Type != "ENCRYPTED PRIVATE KEY" || len(block.Headers) != 0 || len(bytes.TrimSpace(rest)) != 0 || !bytes.HasPrefix(data, []byte("-----BEGIN ENCRYPTED PRIVATE KEY-----")) {
		return errors.New("expected one encrypted PKCS8 key")
	}
	var key encryptedKeyInfo
	if err := unmarshalExact(block.Bytes, &key); err != nil {
		return err
	}
	if !key.Algorithm.Algorithm.Equal(oidPBES2) || len(key.Data) < 16 || len(key.Data) > 16384 {
		return errors.New("unsupported protected key profile")
	}
	var params pkcs.PBES2Params
	if err := unmarshalExact(key.Algorithm.Parameters.FullBytes, &params); err != nil {
		return err
	}
	if !params.KeyDerivationFunc.Algorithm.Equal(oidScrypt) || !params.EncryptionScheme.Algorithm.Equal(oidAES256GCM) {
		return errors.New("unsupported protected key algorithms")
	}
	var kdf scryptParameters
	if err := unmarshalExact(params.KeyDerivationFunc.Parameters.FullBytes, &kdf); err != nil {
		return err
	}
	if len(kdf.Salt) != 32 || kdf.N != scryptN || kdf.R != 8 || kdf.P != 1 || (kdf.KeyLength != 0 && kdf.KeyLength != 32) {
		return errors.New("unsupported protected key derivation cost")
	}
	var gcm gcmParameters
	if err := unmarshalExact(params.EncryptionScheme.Parameters.FullBytes, &gcm); err != nil {
		return err
	}
	if len(gcm.Nonce) != 12 || gcm.ICVLen != 16 {
		return errors.New("unsupported protected key authentication parameters")
	}
	return nil
}
func unlockKey(data []byte, passphrase string) (*rsa.PrivateKey, error) {
	if len(passphrase) == 0 {
		return nil, errors.New("encryption passphrase is required")
	}
	if err := validateKeyEnvelope(data); err != nil {
		return nil, err
	}
	key, err := object.ParsePrivateKeyFromPem(data, []byte(passphrase))
	if err != nil {
		return nil, errors.New("cannot unlock protected volume key")
	}
	rsaKey, ok := key.(*rsa.PrivateKey)
	if !ok || rsaKey.N.BitLen() != 3072 {
		return nil, errors.New("unsupported volume key")
	}
	if err := rsaKey.Validate(); err != nil {
		return nil, errors.New("invalid volume key")
	}
	return rsaKey, nil
}
func generateKey(passphrase string) ([]byte, error) {
	if passphrase == "" {
		return nil, errors.New("encryption passphrase is required")
	}
	key, err := rsa.GenerateKey(rand.Reader, 3072)
	if err != nil {
		return nil, err
	}
	der, err := pkcs8.MarshalPrivateKey(key, []byte(passphrase), pkcs.NewPBESEncrypter(pkcs.AES256GCM, pkcs.NewScryptOpts(32, scryptN, 8, 1)))
	if err != nil {
		return nil, errors.New("protect volume key failed")
	}
	data := pem.EncodeToMemory(&pem.Block{Type: "ENCRYPTED PRIVATE KEY", Bytes: der})
	if err = validateKeyEnvelope(data); err != nil {
		return nil, err
	}
	return data, nil
}
