// NTLM constants and exchange logic reviewed from macos-fuse-t/go-smb2,
// commit 277a9300411249a881a05f7a910f5a83ae3395f2, originally Hiroshi Ioka's go-smb2.
// Modified for s3-smb, 2026. See docs/vendored.md.
// See LICENSE and Attributions.txt in this directory for the upstream notices.
package auth

import (
	"bytes"
	"encoding/binary"
	"errors"
	"unicode/utf16"
	"unicode/utf8"
)

const (
	flagUnicode    uint32 = 1 << 0
	flagTarget     uint32 = 1 << 2
	flagSign       uint32 = 1 << 4
	flagSeal       uint32 = 1 << 5
	flagNTLM       uint32 = 1 << 9
	flagAnonymous  uint32 = 1 << 11
	flagAlwaysSign uint32 = 1 << 15
	flagServer     uint32 = 1 << 17
	flagExtended   uint32 = 1 << 19
	flagTargetInfo uint32 = 1 << 23
	flagVersion    uint32 = 1 << 25
	flag128        uint32 = 1 << 29
	flagKeyExch    uint32 = 1 << 30
	flag56         uint32 = 1 << 31

	requiredFlags = flagUnicode | flagNTLM | flagExtended | flagSign | flag128
	offeredFlags  = requiredFlags | flagTarget | flagAlwaysSign | flagTargetInfo | flagVersion | flagKeyExch | flag56
	securityFlags = requiredFlags | flagSeal | flagVersion | flagKeyExch | flag56

	avEnd       uint16 = 0
	avComputer  uint16 = 1
	avDomain    uint16 = 2
	avFlags     uint16 = 6
	avTimestamp uint16 = 7
	avBindings  uint16 = 10
	micPresent  uint32 = 2
)

var (
	littleEndian = binary.LittleEndian
	ntlmVersion  = []byte{10, 0, 0, 0, 0, 0, 0, 15}
)

type ntlmMessage struct {
	fields    [][]byte
	av        map[uint16][]byte
	raw       []byte
	blob      []byte
	flags     uint32
	kind      uint32
	micOffset int
}

// field reads a security buffer with widened offset arithmetic. Zero-length
// fields do not reference a buffer. MaxLen is ignored as MS-NLMP requires.
func field(data []byte, offset, floor int) ([]byte, error) {
	if offset < 0 || offset > len(data)-8 {
		return nil, errToken
	}
	length := uint64(littleEndian.Uint16(data[offset:]))
	if length == 0 {
		return nil, nil
	}
	start := uint64(littleEndian.Uint32(data[offset+4:]))
	end := start + length
	if start < uint64(floor) || end > uint64(len(data)) {
		return nil, errToken
	}
	return data[int(start):int(end)], nil
}

func decodeNTLM(data []byte) (ntlmMessage, error) {
	result := ntlmMessage{raw: data}
	if len(data) < 12 || len(data) > maxTokenSize || !bytes.Equal(data[:8], []byte("NTLMSSP\x00")) {
		return result, errToken
	}
	result.kind = littleEndian.Uint32(data[8:])
	var offsets []int
	var flagOffset, floor int
	switch result.kind {
	case 1:
		flagOffset, floor, offsets = 12, 32, []int{16, 24}
	case 2:
		flagOffset, floor, offsets = 20, 48, []int{12, 40}
	case 3:
		flagOffset, floor, offsets = 60, 64, []int{12, 20, 28, 36, 44, 52}
	default:
		return result, errToken
	}
	if len(data) < floor {
		return result, errToken
	}
	result.flags = littleEndian.Uint32(data[flagOffset:])
	if result.flags&flagVersion != 0 {
		floor += 8
	}
	if len(data) < floor {
		return result, errToken
	}
	result.micOffset = floor
	for _, offset := range offsets {
		value, err := field(data, offset, floor)
		if err != nil {
			return result, err
		}
		result.fields = append(result.fields, value)
	}
	if err := validateNTLMFields(&result); err != nil {
		return result, err
	}
	return result, nil
}

func validateNTLMFields(result *ntlmMessage) error {
	switch result.kind {
	case 1:
		// Negotiate domain and workstation use OEM, not UTF-16.
		return nil
	case 2:
		if _, err := decodeUTF16(result.fields[0]); err != nil {
			return err
		}
		av, err := decodeAV(result.fields[1])
		result.av = av
		return err
	case 3:
		for _, value := range result.fields[2:5] {
			if _, err := decodeUTF16(value); err != nil {
				return err
			}
		}
		// NTLMv1, empty responses and short v2 blobs are not accepted.
		response := result.fields[1]
		if len(response) < 16+28+4+4 {
			return errToken
		}
		result.blob = response[16:]
		blob := result.blob
		if blob[0] != 1 || blob[1] != 1 || !allZero(blob[2:8]) || !allZero(blob[24:28]) || !allZero(blob[len(blob)-4:]) {
			return errToken
		}
		av, err := decodeAV(blob[28 : len(blob)-4])
		result.av = av
		return err
	default:
		return errToken
	}
}

func decodeAV(data []byte) (map[uint16][]byte, error) {
	pairs := make(map[uint16][]byte)
	for len(data) >= 4 {
		id := littleEndian.Uint16(data)
		length := int(littleEndian.Uint16(data[2:]))
		if length > len(data)-4 {
			return nil, errToken
		}
		value := data[4 : 4+length]
		data = data[4+length:]
		if id == avEnd {
			if length != 0 || len(data) != 0 {
				return nil, errToken
			}
			return pairs, nil
		}
		if _, duplicate := pairs[id]; duplicate {
			return nil, errToken
		}
		if err := validateAV(id, value); err != nil {
			return nil, err
		}
		pairs[id] = value
	}
	return nil, errToken
}

func validateAV(id uint16, value []byte) error {
	switch id {
	case 1, 2, 3, 4, 5, 9:
		_, err := decodeUTF16(value)
		return err
	case avFlags:
		if len(value) != 4 {
			return errToken
		}
	case avTimestamp:
		if len(value) != 8 {
			return errToken
		}
	case 8:
		if len(value) != 48 {
			return errToken
		}
	case avBindings:
		if len(value) != 16 {
			return errToken
		}
	}
	return nil
}

func allZero(data []byte) bool {
	for _, value := range data {
		if value != 0 {
			return false
		}
	}
	return true
}

func encodeUTF16(value string) []byte {
	units := utf16.Encode([]rune(value))
	data := make([]byte, len(units)*2)
	for index, unit := range units {
		littleEndian.PutUint16(data[index*2:], unit)
	}
	return data
}

func decodeUTF16(data []byte) (string, error) {
	if len(data)%2 != 0 {
		return "", errToken
	}
	units := make([]uint16, len(data)/2)
	for index := range units {
		units[index] = littleEndian.Uint16(data[index*2:])
	}
	for index := 0; index < len(units); index++ {
		unit := units[index]
		if unit >= 0xd800 && unit <= 0xdbff {
			if index+1 >= len(units) || units[index+1] < 0xdc00 || units[index+1] > 0xdfff {
				return "", errToken
			}
			index++
		} else if unit >= 0xdc00 && unit <= 0xdfff {
			return "", errToken
		}
	}
	return string(utf16.Decode(units)), nil
}

func validateText(value string) error {
	if !utf8.ValidString(value) || bytes.IndexByte([]byte(value), 0) >= 0 || len(encodeUTF16(value)) > 1024 {
		return errors.New("invalid authentication configuration text")
	}
	return nil
}

func putField(data []byte, offset, start int, value []byte) int {
	littleEndian.PutUint16(data[offset:], uint16(len(value)))
	littleEndian.PutUint16(data[offset+2:], uint16(len(value)))
	littleEndian.PutUint32(data[offset+4:], uint32(start))
	return start + copy(data[start:], value)
}

func appendAV(data []byte, id uint16, value []byte) []byte {
	var header [4]byte
	littleEndian.PutUint16(header[:], id)
	littleEndian.PutUint16(header[2:], uint16(len(value)))
	data = append(data, header[:]...)
	return append(data, value...)
}

func newNTLM(kind uint32, size int) []byte {
	data := make([]byte, size)
	copy(data, "NTLMSSP\x00")
	littleEndian.PutUint32(data[8:], kind)
	return data
}

func negotiateMessage() []byte {
	data := newNTLM(1, 40)
	littleEndian.PutUint32(data[12:], offeredFlags)
	copy(data[32:], ntlmVersion)
	return data
}
