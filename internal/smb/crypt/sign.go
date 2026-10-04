package crypt

import (
	"bytes"
	"crypto/subtle"
	"encoding/binary"
	"fmt"
)

const (
	smbHeaderSize = 64
	flagResponse  = 0x00000001
	flagSigned    = 0x00000008
	commandCancel = 0x000c
)

// Sign requires a zero signature field and the signed flag already set.
// It derives the GMAC nonce from the member's header per MS-SMB2 section 3.1.4.1.
// It does not change the supplied header, body or compound padding.
func (protector *Protector) Sign(member []byte) ([16]byte, error) {
	if err := validateMember(member); err != nil {
		return [16]byte{}, err
	}
	var zero [16]byte
	if !bytes.Equal(member[48:64], zero[:]) {
		return [16]byte{}, fmt.Errorf("SMB signature field must be zero before signing")
	}
	return protector.signature(member)
}

// Verify checks the received member in constant time without changing it.
func (protector *Protector) Verify(member []byte) error {
	if err := validateMember(member); err != nil {
		return err
	}
	unsigned := bytes.Clone(member)
	clear(unsigned[48:64])
	expected, err := protector.signature(unsigned)
	if err != nil {
		return err
	}
	if subtle.ConstantTimeCompare(expected[:], member[48:64]) != 1 {
		return fmt.Errorf("invalid SMB signature")
	}
	return nil
}

func validateMember(member []byte) error {
	if len(member) < smbHeaderSize {
		return fmt.Errorf("short SMB signing member")
	}
	if !bytes.Equal(member[:4], []byte{0xfe, 'S', 'M', 'B'}) || binary.LittleEndian.Uint16(member[4:6]) != smbHeaderSize {
		return fmt.Errorf("invalid SMB signing header")
	}
	if binary.LittleEndian.Uint32(member[16:20])&flagSigned == 0 {
		return fmt.Errorf("SMB signed flag is not set")
	}
	return nil
}

func (protector *Protector) signature(member []byte) ([16]byte, error) {
	if protector.signBlock != nil {
		return aesCMAC(protector.signBlock, member), nil
	}
	if protector.signGMAC == nil {
		return [16]byte{}, fmt.Errorf("SMB protector has no signing key")
	}
	var nonce [12]byte
	copy(nonce[:8], member[24:32])
	flags := binary.LittleEndian.Uint32(member[16:20])
	var suffix uint32
	if flags&flagResponse != 0 {
		suffix = 1
	}
	if binary.LittleEndian.Uint16(member[12:14]) == commandCancel {
		suffix |= 2
	}
	binary.LittleEndian.PutUint32(nonce[8:], suffix)
	tag := protector.signGMAC.Seal(nil, nonce[:], nil, member)
	var signature [16]byte
	copy(signature[:], tag)
	return signature, nil
}
