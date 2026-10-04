// NTLM constants and exchange logic reviewed from macos-fuse-t/go-smb2,
// commit 277a9300411249a881a05f7a910f5a83ae3395f2, originally Hiroshi Ioka's go-smb2.
// Modified for s3-smb, 2026. See docs/vendored.md.
// See LICENSE and Attributions.txt in this directory for the upstream notices.
package auth

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math"
	"strings"
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

	requiredFlags = flagUnicode | flagNTLM | flagExtended
	offeredFlags  = requiredFlags | flagSign | flagTarget | flagAlwaysSign | flagTargetInfo | flagVersion | flagKeyExch | flag128 | flag56

	messageNegotiate    uint32 = 1
	messageChallenge    uint32 = 2
	messageAuthenticate uint32 = 3

	avEnd         uint16 = 0
	avComputer    uint16 = 1
	avDomain      uint16 = 2
	avDNSComputer uint16 = 3
	avDNSDomain   uint16 = 4
	avDNSTree     uint16 = 5
	avFlags       uint16 = 6
	avTimestamp   uint16 = 7
	avSingleHost  uint16 = 8
	avTargetName  uint16 = 9
	avBindings    uint16 = 10
	micPresent    uint32 = 2
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

// hasMIC is called only on decoded messages, whose AV lengths are validated.
func (message ntlmMessage) hasMIC() bool {
	flags, exists := message.av[avFlags]
	return exists && littleEndian.Uint32(flags)&micPresent != 0
}

// field reads a security buffer with widened offset arithmetic. Zero-length
// fields do not reference a buffer. MaxLen is ignored as MS-NLMP requires.
func field(data []byte, offset, floor int) ([]byte, error) {
	if offset < 0 || floor < 0 || offset > len(data)-8 {
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
	return data[start:end], nil
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
	case messageNegotiate:
		flagOffset, floor, offsets = 12, 32, []int{16, 24}
	case messageChallenge:
		flagOffset, floor, offsets = 20, 48, []int{12, 40}
	case messageAuthenticate:
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
	ignoreSessionKey := result.kind == messageAuthenticate && result.flags&flagKeyExch == 0
	fields, err := readNTLMFields(data, offsets, floor, ignoreSessionKey)
	if err != nil {
		return result, err
	}
	result.fields = fields
	if err := validateNTLMFields(&result); err != nil {
		return result, err
	}
	if result.kind == messageAuthenticate && result.hasMIC() {
		if len(data) < floor+16 {
			return result, errToken
		}
		if _, err := readNTLMFields(data, offsets, floor+16, ignoreSessionKey); err != nil {
			return result, err
		}
	}
	return result, nil
}

func readNTLMFields(data []byte, offsets []int, floor int, ignoreSessionKey bool) ([][]byte, error) {
	values := make([][]byte, 0, len(offsets))
	for _, offset := range offsets {
		// MS-NLMP 2.2.1.3 requires ignoring this buffer when KEY_EXCH is clear.
		if ignoreSessionKey && offset == 52 {
			values = append(values, nil)
			continue
		}
		value, err := field(data, offset, floor)
		if err != nil {
			return nil, err
		}
		if len(value) != 0 {
			start := uint64(littleEndian.Uint32(data[offset+4:]))
			for earlier, previous := range values {
				previousStart := uint64(littleEndian.Uint32(data[offsets[earlier]+4:]))
				if len(previous) != 0 && start < previousStart+uint64(len(previous)) && previousStart < start+uint64(len(value)) {
					return nil, errToken
				}
			}
		}
		values = append(values, value)
	}
	return values, nil
}

func validateNTLMFields(result *ntlmMessage) error {
	switch result.kind {
	case messageNegotiate:
		// Negotiate domain and workstation use OEM, not UTF-16.
		return nil
	case messageChallenge:
		if _, err := decodeUTF16(result.fields[0]); err != nil {
			return err
		}
		av, err := decodeAV(result.fields[1])
		result.av = av
		return err
	case messageAuthenticate:
		if len(result.fields[0]) != 0 && len(result.fields[0]) != 24 {
			return errToken
		}
		if result.flags&flagKeyExch != 0 && len(result.fields[5]) != 16 {
			return errToken
		}
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
		if blob[0] != 1 || blob[1] != 1 || !allZero(blob[len(blob)-4:]) {
			return errToken
		}
		// MS-NLMP 2.2.2.7 requires ignoring Reserved1, Reserved2 and
		// Reserved3 on receipt. They remain covered by the proof and MIC.
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
	case avComputer, avDomain, avDNSComputer, avDNSDomain, avDNSTree, avTargetName:
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
	case avSingleHost:
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
	if !utf8.ValidString(value) || strings.IndexByte(value, 0) >= 0 || len(encodeUTF16(value)) > 1024 {
		return errors.New("invalid authentication configuration text")
	}
	return nil
}

func putField(data []byte, offset, start int, value []byte) (int, error) {
	size := len(value)
	if offset < 0 || offset > len(data)-8 || start < 0 || start > math.MaxUint32 || start > len(data) || size > len(data)-start || size > math.MaxUint16 {
		return 0, errToken
	}
	littleEndian.PutUint16(data[offset:], uint16(size))
	littleEndian.PutUint16(data[offset+2:], uint16(size))
	littleEndian.PutUint32(data[offset+4:], uint32(start))
	return start + copy(data[start:], value), nil
}

func appendAV(data []byte, id uint16, value []byte) ([]byte, error) {
	size := len(value)
	if size > math.MaxUint16 || len(data)+4+size > maxTokenSize {
		return nil, errToken
	}
	var header [4]byte
	littleEndian.PutUint16(header[:], id)
	littleEndian.PutUint16(header[2:], uint16(size))
	data = append(data, header[:]...)
	return append(data, value...), nil
}

type avPair struct {
	value []byte
	id    uint16
}

func encodeAV(pairs []avPair) ([]byte, error) {
	var data []byte
	for _, pair := range pairs {
		var err error
		data, err = appendAV(data, pair.id, pair.value)
		if err != nil {
			return nil, err
		}
	}
	return appendAV(data, avEnd, nil)
}

func targetInfoWithMIC(info []byte) ([]byte, error) {
	copyInfo := bytes.Clone(info)
	pairs, err := decodeAV(copyInfo)
	if err != nil {
		return nil, err
	}
	if flags, exists := pairs[avFlags]; exists {
		littleEndian.PutUint32(flags, littleEndian.Uint32(flags)|micPresent)
		return copyInfo, nil
	}
	var flags [4]byte
	littleEndian.PutUint32(flags[:], micPresent)
	withFlags, err := appendAV(copyInfo[:len(copyInfo)-4], avFlags, flags[:])
	if err != nil {
		return nil, err
	}
	return appendAV(withFlags, avEnd, nil)
}

func newNTLM(kind uint32, size int) []byte {
	data := make([]byte, size)
	copy(data, "NTLMSSP\x00")
	littleEndian.PutUint32(data[8:], kind)
	return data
}

func negotiateMessage() []byte {
	data := newNTLM(messageNegotiate, 40)
	littleEndian.PutUint32(data[12:], offeredFlags)
	copy(data[32:], ntlmVersion)
	return data
}
