package server

import (
	"errors"
	"fmt"
	"os/exec"
	"testing"
)

// This needs no file handlers or main wiring. The CI test image supplies the
// Samba client; developer machines without it skip this interoperability test.
func TestSmbclientAuthenticatesAndConnectsWithoutListing(t *testing.T) {
	if _, err := exec.LookPath("smbclient"); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			t.Skip("smbclient is not installed")
		}
		t.Fatal(err)
	}
	for _, encrypted := range []bool{false, true} {
		t.Run(fmt.Sprintf("encrypted_%t", encrypted), func(t *testing.T) { checkSmbclientLogin(t, encrypted) })
	}
}
