package auth

import (
	"encoding/asn1"
	"errors"
)

var (
	spnegoOID = asn1.ObjectIdentifier{1, 3, 6, 1, 5, 5, 2}
	ntlmOID   = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 311, 2, 2, 10}
)

const maxTokenSize = 65535

var errToken = errors.New("malformed authentication token")

type spnegoToken struct {
	mechs     []asn1.ObjectIdentifier
	mechanism asn1.ObjectIdentifier
	mechList  []byte
	token     []byte
	mic       []byte
	state     int
	initial   bool
}

// readDER consumes one value. Callers check class, tag and construction at each
// fixed level; no recursive walk or BER indefinite lengths are accepted.
func readDER(data []byte) (asn1.RawValue, []byte, error) {
	var value asn1.RawValue
	rest, err := asn1.Unmarshal(data, &value)
	if err != nil {
		return value, nil, errToken
	}
	return value, rest, nil
}

func unwrapDER(data []byte, class, tag int, compound bool) ([]byte, error) {
	value, rest, err := readDER(data)
	if err != nil || len(rest) != 0 || value.Class != class || value.Tag != tag || value.IsCompound != compound {
		return nil, errToken
	}
	return value.Bytes, nil
}

func unmarshalDER(data []byte, value any) error {
	rest, err := asn1.Unmarshal(data, value)
	if err != nil || len(rest) != 0 {
		return errToken
	}
	return nil
}

func decodeSPNEGO(data []byte) (spnegoToken, error) {
	result := spnegoToken{state: -1}
	if len(data) == 0 || len(data) > maxTokenSize {
		return result, errToken
	}
	outer, rest, err := readDER(data)
	if err != nil || len(rest) != 0 || !outer.IsCompound {
		return result, errToken
	}
	if outer.Class == asn1.ClassApplication && outer.Tag == 0 {
		oid, remaining, oidErr := readDER(outer.Bytes)
		if oidErr != nil {
			return result, oidErr
		}
		var mechanism asn1.ObjectIdentifier
		if err := unmarshalDER(oid.FullBytes, &mechanism); err != nil || !mechanism.Equal(spnegoOID) {
			return result, errToken
		}
		outer, rest, err = readDER(remaining)
		if err != nil || len(rest) != 0 || outer.Class != asn1.ClassContextSpecific || outer.Tag != 0 || !outer.IsCompound {
			return result, errToken
		}
		result.initial = true
	} else if outer.Class != asn1.ClassContextSpecific || outer.Tag != 1 {
		return result, errToken
	}
	fields, err := unwrapDER(outer.Bytes, asn1.ClassUniversal, asn1.TagSequence, true)
	if err != nil {
		return result, err
	}
	if err := decodeSPNEGOFields(fields, &result); err != nil {
		return result, err
	}
	if result.initial && len(result.mechs) == 0 {
		return result, errToken
	}
	return result, nil
}

func decodeSPNEGOFields(data []byte, result *spnegoToken) error {
	previous := -1
	for len(data) != 0 {
		field, rest, err := readDER(data)
		if err != nil || field.Class != asn1.ClassContextSpecific || !field.IsCompound || field.Tag <= previous || field.Tag > 4 {
			return errToken
		}
		previous = field.Tag
		if result.initial {
			err = decodeInitField(field, result)
		} else {
			err = decodeResponseField(field, result)
		}
		if err != nil {
			return err
		}
		data = rest
	}
	return nil
}

func decodeInitField(field asn1.RawValue, result *spnegoToken) error {
	switch field.Tag {
	case 0:
		result.mechList = field.Bytes
		if err := unmarshalDER(field.Bytes, &result.mechs); err != nil {
			return err
		}
		if len(result.mechs) > 16 {
			return errToken
		}
		for index, mech := range result.mechs {
			for _, earlier := range result.mechs[:index] {
				if earlier.Equal(mech) {
					return errToken
				}
			}
		}
	case 1:
		var flags asn1.BitString
		return unmarshalDER(field.Bytes, &flags)
	case 2:
		return unmarshalDER(field.Bytes, &result.token)
	case 3:
		// MS-SPNG NegTokenInit2 uses tag 3 for hints instead of a MIC.
		if len(field.Bytes) != 0 && field.Bytes[0] == 0x30 {
			return decodeHints(field.Bytes)
		}
		return unmarshalDER(field.Bytes, &result.mic)
	case 4:
		return unmarshalDER(field.Bytes, &result.mic)
	default:
		return errToken
	}
	return nil
}

func decodeHints(data []byte) error {
	fields, err := unwrapDER(data, asn1.ClassUniversal, asn1.TagSequence, true)
	if err != nil {
		return err
	}
	previous := -1
	for len(fields) != 0 {
		field, rest, err := readDER(fields)
		if err != nil || field.Class != asn1.ClassContextSpecific || !field.IsCompound || field.Tag <= previous || field.Tag > 1 {
			return errToken
		}
		previous = field.Tag
		tag := asn1.TagGeneralString
		if field.Tag == 1 {
			tag = asn1.TagOctetString
		}
		if _, err := unwrapDER(field.Bytes, asn1.ClassUniversal, tag, false); err != nil {
			return err
		}
		fields = rest
	}
	return nil
}

func decodeResponseField(field asn1.RawValue, result *spnegoToken) error {
	switch field.Tag {
	case 0:
		var state asn1.Enumerated
		if err := unmarshalDER(field.Bytes, &state); err != nil || state < 0 || state > 3 {
			return errToken
		}
		result.state = int(state)
	case 1:
		return unmarshalDER(field.Bytes, &result.mechanism)
	case 2:
		return unmarshalDER(field.Bytes, &result.token)
	case 3:
		return unmarshalDER(field.Bytes, &result.mic)
	default:
		return errToken
	}
	return nil
}

func wrapDER(class, tag int, data []byte) ([]byte, error) {
	return asn1.Marshal(asn1.RawValue{Class: class, Tag: tag, IsCompound: true, Bytes: data})
}

func encodeInitial(token []byte) ([]byte, error) {
	fields := struct {
		Mechs []asn1.ObjectIdentifier `asn1:"explicit,tag:0"`
		Token []byte                  `asn1:"optional,explicit,tag:2"`
	}{Mechs: []asn1.ObjectIdentifier{ntlmOID}, Token: token}
	sequence, err := asn1.Marshal(fields)
	if err != nil {
		return nil, err
	}
	choice, err := wrapDER(asn1.ClassContextSpecific, 0, sequence)
	if err != nil {
		return nil, err
	}
	oid, err := asn1.Marshal(spnegoOID)
	if err != nil {
		return nil, err
	}
	return wrapDER(asn1.ClassApplication, 0, append(oid, choice...))
}

func encodeResponse(state int, mechanism asn1.ObjectIdentifier, token, mic []byte) ([]byte, error) {
	// A RawValue keeps an absent state distinct from accept-completed (zero).
	var stateValue asn1.RawValue
	if state >= 0 {
		encoded, err := asn1.Marshal(asn1.Enumerated(state))
		if err != nil {
			return nil, err
		}
		stateValue = asn1.RawValue{Class: asn1.ClassContextSpecific, Tag: 0, IsCompound: true, Bytes: encoded}
	}
	fields := struct {
		State     asn1.RawValue         `asn1:"optional"`
		Mechanism asn1.ObjectIdentifier `asn1:"optional,explicit,tag:1"`
		Token     []byte                `asn1:"optional,explicit,tag:2"`
		MIC       []byte                `asn1:"optional,explicit,tag:3"`
	}{State: stateValue, Mechanism: mechanism, Token: token, MIC: mic}
	sequence, err := asn1.Marshal(fields)
	if err != nil {
		return nil, err
	}
	return wrapDER(asn1.ClassContextSpecific, 1, sequence)
}
