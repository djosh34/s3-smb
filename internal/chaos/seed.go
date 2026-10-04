// SPDX-License-Identifier: AGPL-3.0-only

// Package chaos provides replayable faults and checks for chaos tests.
package chaos

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	mathrand "math/rand/v2"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// Seed returns S3_SMB_CHAOS_SEED, or a random seed, and logs a replay command.
// The environment value is an unsigned decimal integer.
func Seed(t testing.TB) uint64 {
	t.Helper()
	seed, err := seedValue(os.Getenv("S3_SMB_CHAOS_SEED"), rand.Read)
	if err != nil {
		t.Fatalf("chaos seed: %v", err)
		return 0
	}
	t.Logf("chaos seed %d; replay: %s", seed, replayCommand(seed, t.Name()))
	return seed
}

func replayCommand(seed uint64, name string) string {
	parts := strings.Split(name, "/")
	for i, part := range parts {
		parts[i] = "^" + regexp.QuoteMeta(part) + "$"
	}
	pattern := strings.ReplaceAll(strings.Join(parts, "/"), "'", "'\"'\"'")
	return fmt.Sprintf("S3_SMB_CHAOS_SEED=%d go test -race -shuffle=on -count=1 -v ./test/e2e -run '%s'", seed, pattern)
}

func seedValue(value string, random func([]byte) (int, error)) (uint64, error) {
	if value != "" {
		seed, err := strconv.ParseUint(value, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("parse S3_SMB_CHAOS_SEED: %w", err)
		}
		return seed, nil
	}
	var data [8]byte
	n, err := random(data[:])
	if err != nil {
		return 0, fmt.Errorf("read random seed: %w", err)
	}
	if n != len(data) {
		return 0, fmt.Errorf("read random seed: got %d bytes, want %d", n, len(data))
	}
	return binary.LittleEndian.Uint64(data[:]), nil
}

// Rand returns a repeatable generator for a named stream of seed. The caller
// owns it; a generator must not be shared between concurrent goroutines.
func Rand(seed uint64, stream string) *mathrand.Rand {
	data := make([]byte, 8+len(stream))
	binary.LittleEndian.PutUint64(data, seed)
	copy(data[8:], stream)
	hash := sha256.Sum256(data)
	//nolint:gosec // Chaos streams must be repeatable, not cryptographically random.
	return mathrand.New(mathrand.NewPCG(binary.LittleEndian.Uint64(hash[:8]), binary.LittleEndian.Uint64(hash[8:16])))
}
