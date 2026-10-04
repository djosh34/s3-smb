// SPDX-License-Identifier: AGPL-3.0-only

package e2e

import (
	"errors"
	"io/fs"
	"testing"

	"github.com/djosh34/s3-smb/internal/chaos"
	"github.com/djosh34/s3-smb/internal/smb"
	smbclient "github.com/hirochachacha/go-smb2"
)

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
