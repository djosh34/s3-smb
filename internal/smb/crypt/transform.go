package crypt

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
)

const transformHeaderSize = 52

// Seal builds a GCM transform for this session and role. It leaves plaintext
// unchanged and does not sign it. Each successful call reserves a distinct
// nonce, including concurrent sends and async replies. Exhaustion is permanent
// and requires closing the session.
func (protector *Protector) Seal(plaintext []byte) ([]byte, error) {
	if protector.send == nil {
		return nil, fmt.Errorf("SMB session does not support encryption")
	}
	size := uint64(len(plaintext))
	if size < smbHeaderSize || size > math.MaxUint32 {
		return nil, fmt.Errorf("invalid SMB plaintext size %d", len(plaintext))
	}
	nonce, err := protector.nextNonce()
	if err != nil {
		return nil, err
	}
	header := make([]byte, transformHeaderSize)
	copy(header[:4], []byte{0xfd, 'S', 'M', 'B'})
	copy(header[20:32], nonce[:])
	binary.LittleEndian.PutUint32(header[36:40], uint32(size))
	binary.LittleEndian.PutUint16(header[42:44], 1)
	binary.LittleEndian.PutUint64(header[44:52], protector.sessionID)
	sealed := protector.send.Seal(nil, nonce[:], plaintext, header[20:52])
	copy(header[4:20], sealed[len(plaintext):])
	return append(header, sealed[:len(plaintext)]...), nil
}

func (protector *Protector) nextNonce() ([12]byte, error) {
	protector.nonceMu.Lock()
	defer protector.nonceMu.Unlock()
	if protector.counter == math.MaxUint64 {
		return [12]byte{}, fmt.Errorf("SMB encryption nonce counter exhausted")
	}
	protector.counter++
	var nonce [12]byte
	copy(nonce[:4], protector.seed[:])
	binary.LittleEndian.PutUint64(nonce[4:], protector.counter)
	return nonce, nil
}

// Open validates the session, size, reserved fields, nonce padding and tag.
// It rejects wrong-direction keys. Authentication happens before any plaintext
// is returned to wire decoding or server dispatch. Input is never changed.
func (protector *Protector) Open(transform []byte) ([]byte, error) {
	if protector.receive == nil {
		return nil, fmt.Errorf("SMB session does not support decryption")
	}
	if err := validateTransform(transform, protector.sessionID); err != nil {
		return nil, err
	}
	sealed := make([]byte, len(transform)-transformHeaderSize, len(transform)-transformHeaderSize+16)
	copy(sealed, transform[transformHeaderSize:])
	sealed = append(sealed, transform[4:20]...)
	plaintext, err := protector.receive.Open(nil, transform[20:32], sealed, transform[20:52])
	if err != nil {
		return nil, fmt.Errorf("authenticate SMB transform: %w", err)
	}
	return plaintext, nil
}

// validateTransform checks bounds before every header access and does not
// allocate based on OriginalMessageSize. GCM authenticates bytes 20 through 51.
func validateTransform(transform []byte, sessionID uint64) error {
	if len(transform) < transformHeaderSize+smbHeaderSize {
		return fmt.Errorf("short SMB transform")
	}
	if !bytes.Equal(transform[:4], []byte{0xfd, 'S', 'M', 'B'}) {
		return fmt.Errorf("invalid SMB transform protocol identifier")
	}
	if binary.LittleEndian.Uint64(transform[44:52]) != sessionID {
		return fmt.Errorf("SMB transform belongs to another session")
	}
	if uint64(binary.LittleEndian.Uint32(transform[36:40])) != uint64(len(transform))-transformHeaderSize {
		return fmt.Errorf("SMB transform size mismatch")
	}
	if binary.LittleEndian.Uint16(transform[40:42]) != 0 || binary.LittleEndian.Uint16(transform[42:44]) != 1 {
		return fmt.Errorf("invalid SMB transform reserved field or flags")
	}
	var zero [4]byte
	if !bytes.Equal(transform[32:36], zero[:]) {
		return fmt.Errorf("invalid SMB GCM nonce padding")
	}
	return nil
}
