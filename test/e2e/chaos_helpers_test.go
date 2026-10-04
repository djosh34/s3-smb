// SPDX-License-Identifier: AGPL-3.0-only

package e2e

import (
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/djosh34/s3-smb/internal/chaos"
	"github.com/djosh34/s3-smb/internal/smb"
	smbclient "github.com/hirochachacha/go-smb2"
)

// newChaosFixture uses only the explicit race-enabled chaos daemon. Ordinary
// package runs skip daemon-backed chaos scenarios; the dedicated CI step sets it.
func newChaosFixture(t *testing.T, encrypted bool) *fixture {
	t.Helper()
	binary := os.Getenv("S3_SMB_CHAOS_BINARY")
	if binary == "" {
		t.Skip("needs S3_SMB_CHAOS_BINARY")
	}
	return newFixtureWithBinary(t, encrypted, binary)
}

// chaosRead adapts STATUS_NO_SUCH_FILE to the ledger contract. The client
// already maps missing-name and missing-path statuses to fs.ErrNotExist.
// Other failures, including timeouts and access errors, must still fail a check.
func chaosRead(read chaos.ReadFunc) chaos.ReadFunc {
	return func(name string) ([]byte, error) {
		data, err := read(name)
		var response *smbclient.ResponseError
		if errors.As(err, &response) && response != nil && response.Code == uint32(smb.StatusNoSuchFile) {
			return data, errors.Join(err, fs.ErrNotExist)
		}
		return data, err
	}
}

func TestChaosRead(t *testing.T) {
	missing := &fs.PathError{Op: "open", Path: "gone", Err: &smbclient.ResponseError{Code: uint32(smb.StatusNoSuchFile)}}
	missingRead := chaosRead(func(string) ([]byte, error) { return nil, missing })
	_, readErr := missingRead("gone")
	if !errors.Is(readErr, fs.ErrNotExist) || !errors.Is(readErr, missing) {
		t.Fatalf("missing-file status: %v", readErr)
	}
	failure := errors.New("transport failed")
	for _, original := range []error{nil, failure, fs.ErrNotExist, &fs.PathError{Op: "open", Path: "gone", Err: fs.ErrNotExist}, &smbclient.ResponseError{Code: uint32(smb.StatusAccessDenied)}} {
		read := chaosRead(func(string) ([]byte, error) { return []byte("data"), original })
		data, err := read("band")
		if string(data) != "data" || err != original {
			t.Fatalf("changed ordinary read result: %q %v", data, err)
		}
	}
}

func TestChaosFixtureBinary(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)
	t.Setenv("S3_SMB_E2E_ENDPOINT", server.URL)
	t.Setenv("S3_SMB_E2E_BINARY", "")
	t.Setenv("S3_SMB_CHAOS_BINARY", "/explicit/race-daemon")
	f := newChaosFixture(t, true)
	if f.binary != "/explicit/race-daemon" || !f.encrypted {
		t.Fatalf("binary=%q, encrypted=%t", f.binary, f.encrypted)
	}
	if os.Getenv("S3_SMB_E2E_BINARY") != "" {
		t.Fatal("chaos fixture changed the default daemon environment")
	}
	f.freshLocal()
	if f.binary != "/explicit/race-daemon" {
		t.Fatal("cold recovery lost the selected binary")
	}
	t.Setenv("S3_SMB_E2E_BINARY", "/default/daemon")
	ordinary := newFixture(t, false)
	if ordinary.binary != "/default/daemon" {
		t.Fatal("ordinary fixture selected the chaos daemon")
	}
}

func TestChaosFixtureSkipsWithoutBinary(t *testing.T) {
	ran := false
	t.Run("no binary", func(t *testing.T) {
		t.Setenv("S3_SMB_CHAOS_BINARY", "")
		t.Setenv("S3_SMB_E2E_BINARY", "/default/daemon")
		newChaosFixture(t, false)
		ran = true
	})
	if ran {
		t.Fatal("chaos fixture used a default daemon when its binary was unset")
	}
}

func TestChaosClientAddress(t *testing.T) {
	f := &fixture{addr: "daemon"}
	if got := f.connectAddr(); got != "daemon" {
		t.Fatal(got)
	}
	f.clientAddr = "proxy"
	if got := f.connectAddr(); got != "proxy" || f.addr != "daemon" {
		t.Fatalf("dial address %q, daemon address %q", got, f.addr)
	}
}
