// SPDX-License-Identifier: AGPL-3.0-only

package chaos

import (
	"errors"
	"testing"
)

func TestSeed(t *testing.T) {
	t.Setenv("S3_SMB_CHAOS_SEED", "18446744073709551615")
	if got := Seed(t); got != ^uint64(0) {
		t.Fatal(got)
	}
}

func TestSeedValue(t *testing.T) {
	for _, value := range []string{"-1", "abc", "18446744073709551616", "0x123", " 1"} {
		if _, err := seedValue(value, nil); err == nil {
			t.Fatalf("accepted %q", value)
		}
	}
	if got, err := seedValue("0", nil); err != nil || got != 0 {
		t.Fatalf("zero seed: %d %v", got, err)
	}
	got, err := seedValue("", func(data []byte) (int, error) {
		data[0] = 42
		return len(data), nil
	})
	if err != nil || got != 42 {
		t.Fatalf("random seed: %d %v", got, err)
	}
	failure := errors.New("random failed")
	if _, err := seedValue("", func([]byte) (int, error) { return 0, failure }); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	if _, err := seedValue("", func([]byte) (int, error) { return 1, nil }); err == nil {
		t.Fatal("accepted short entropy read")
	}
}

func TestReplayCommand(t *testing.T) {
	got := replayCommand(349, "TestChaos/s3[cut]/client's_write")
	want := `S3_SMB_CHAOS_SEED=349 go test -race -shuffle=on -count=1 -v ./test/e2e -run '^TestChaos$/^s3\[cut\]$/^client'"'"'s_write$'`
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestRandStreams(t *testing.T) {
	a, b := Rand(349, "faults"), Rand(349, "faults")
	c, d := Rand(349, "data"), Rand(350, "faults")
	for range 100 {
		value := a.Uint64()
		if value != b.Uint64() {
			t.Fatal("same stream diverged")
		}
		if value == c.Uint64() || value == d.Uint64() {
			t.Fatal("different streams collided")
		}
	}
}
