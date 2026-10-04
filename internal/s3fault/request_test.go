// SPDX-License-Identifier: AGPL-3.0-only

package s3fault

import (
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type drainingBody struct {
	io.Reader
	release chan struct{}
}

func (body *drainingBody) Close() error {
	<-body.release
	return nil
}

func TestCutRequestCloseDoesNotDrain(t *testing.T) {
	release := make(chan struct{})
	body := &cutRequestBody{
		ReadCloser: &drainingBody{Reader: strings.NewReader("abcde"), release: release},
		proxy:      &Proxy{events: make(chan Event, 1)}, request: httptest.NewRequestWithContext(t.Context(), "PUT", "/bucket/chunks/key", nil),
		remaining: 4,
	}
	prefix := make([]byte, 4)
	if _, err := io.ReadFull(body, prefix); err != nil {
		t.Fatal(err)
	}
	if _, err := body.Read(prefix); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("cut read: %v", err)
	}
	done := make(chan struct{})
	var closeErr error
	go func() {
		closeErr = body.Close()
		close(done)
	}()
	defer func() {
		close(release)
		<-done
	}()
	select {
	case <-done:
		if closeErr != nil {
			t.Fatal(closeErr)
		}
	case <-time.After(time.Second):
		t.Error("closing a cut request waited for unread client bytes")
	}
}
