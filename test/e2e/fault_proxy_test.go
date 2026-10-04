// SPDX-License-Identifier: AGPL-3.0-only
package e2e

import (
	"testing"

	"github.com/djosh34/s3-smb/internal/s3fault"
)

func newFaultProxy(t *testing.T, upstream string) *s3fault.Proxy {
	t.Helper()
	proxy, err := s3fault.New(t.Context(), upstream)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := proxy.Close(); err != nil {
			t.Error(err)
		}
	})
	return proxy
}
