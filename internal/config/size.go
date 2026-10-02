// SPDX-License-Identifier: AGPL-3.0-only
package config

import (
	"errors"
	"math/big"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// ByteSize is a capacity in bytes. A nil *ByteSize means the JuiceFS default,
// and zero disables the disk cache.
type ByteSize int64

var decimalSize = regexp.MustCompile(`^([0-9]+(?:\.[0-9]+)?)\s*(B|KB|MB|GB|TB)?$`)

// ParseByteSize accepts decimal units, rounding fractional bytes upward so that
// every positive input stays positive. Binary units and exponent notation fail.
func ParseByteSize(s string) (ByteSize, error) {
	m := decimalSize.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0, errors.New("capacity must be nonnegative decimal bytes or B, KB, MB, GB, TB")
	}
	n, ok := new(big.Rat).SetString(m[1])
	if !ok {
		return 0, errors.New("invalid capacity")
	}
	multiplier := int64(1)
	switch m[2] {
	case "KB":
		multiplier = 1000
	case "MB":
		multiplier = 1000000
	case "GB":
		multiplier = 1000000000
	case "TB":
		multiplier = 1000000000000
	}
	n.Mul(n, new(big.Rat).SetInt64(multiplier))
	bytes, remainder := new(big.Int), new(big.Int)
	bytes.QuoRem(n.Num(), n.Denom(), remainder)
	if remainder.Sign() > 0 {
		bytes.Add(bytes, big.NewInt(1))
	}
	if !bytes.IsInt64() {
		return 0, errors.New("capacity overflows signed 64-bit bytes")
	}
	return ByteSize(bytes.Int64()), nil
}
func (b *ByteSize) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode || (node.Tag != "!!str" && node.Tag != "!!int" && node.Tag != "!!float") {
		return errors.New("capacity must be a decimal size")
	}
	v, err := ParseByteSize(node.Value)
	if err == nil {
		*b = v
	}
	return err
}
