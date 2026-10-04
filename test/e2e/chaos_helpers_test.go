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

// chaosRead adapts the SMB client's missing-file statuses to the ledger contract.
// Other failures, including timeouts and access errors, must still fail a check.
func chaosRead(read chaos.ReadFunc) chaos.ReadFunc {
	return func(name string) ([]byte, error) {
		data, err := read(name)
		var response *smbclient.ResponseError
		if errors.As(err, &response) && response != nil &&
			(response.Code == uint32(smb.StatusNoSuchFile) || response.Code == uint32(smb.StatusObjectNameNotFound) || response.Code == uint32(smb.StatusObjectPathNotFound)) {
			return data, errors.Join(err, fs.ErrNotExist)
		}
		return data, err
	}
}

func TestChaosRead(t *testing.T) {
	for _, code := range []smb.Status{smb.StatusNoSuchFile, smb.StatusObjectNameNotFound, smb.StatusObjectPathNotFound} {
		original := &fs.PathError{Op: "open", Path: "gone", Err: &smbclient.ResponseError{Code: uint32(code)}}
		read := chaosRead(func(string) ([]byte, error) { return nil, original })
		_, err := read("gone")
		if !errors.Is(err, fs.ErrNotExist) || !errors.Is(err, original) {
			t.Fatalf("missing status %x: %v", code, err)
		}
	}
	failure := errors.New("transport failed")
	for _, original := range []error{nil, failure, fs.ErrNotExist, &smbclient.ResponseError{Code: uint32(smb.StatusAccessDenied)}} {
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
