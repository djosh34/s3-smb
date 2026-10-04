// SPDX-License-Identifier: AGPL-3.0-only
package storage

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/djosh34/s3-smb/internal/juicefs/pkg/object"
)

// fakeS3 answers every request with one XML error.
func fakeS3(t *testing.T, check func(*http.Request) (int, string)) object.ObjectStorage {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		status, code := check(r)
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(status)
		if _, err := io.WriteString(w, "<Error><Code>"+code+"</Code><Message>fixture</Message></Error>"); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(srv.Close)
	raw, err := object.NewS3(object.S3Options{Bucket: "fixture", Endpoint: srv.URL, AccessKey: "synthetic", SecretKey: "synthetic"})
	if err != nil {
		t.Fatal(err)
	}
	if closer, ok := raw.(io.Closer); ok {
		t.Cleanup(func() {
			if closeErr := closer.Close(); closeErr != nil {
				t.Error(closeErr)
			}
		})
	}
	return raw
}

func TestS3MissingIsNotAuthenticationFailure(t *testing.T) {
	raw := fakeS3(t, func(r *http.Request) (int, string) {
		if strings.HasSuffix(r.URL.Path, "missing") {
			return http.StatusNotFound, "NoSuchKey"
		}
		return http.StatusForbidden, "AccessDenied"
	})
	if _, err := raw.Get(context.Background(), "missing", 0, -1); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing: %v", err)
	}
	if _, err := raw.Get(context.Background(), "denied", 0, -1); err == nil || errors.Is(err, os.ErrNotExist) {
		t.Fatalf("denied access mistaken for a missing object: %v", err)
	}
}

func TestS3ConditionalPublicationHeader(t *testing.T) {
	raw := fakeS3(t, func(r *http.Request) (int, string) {
		if r.Method != http.MethodPut || r.Header.Get("If-None-Match") != "*" {
			t.Errorf("unconditional publication: %s %q", r.Method, r.Header.Get("If-None-Match"))
		}
		return http.StatusPreconditionFailed, "PreconditionFailed"
	})
	conditional, ok := raw.(interface {
		PutIfAbsent(context.Context, string, io.Reader) error
	})
	if !ok {
		t.Fatal("S3 client has no conditional PUT")
	}
	if err := conditional.PutIfAbsent(context.Background(), "keep", strings.NewReader("data")); err == nil {
		t.Fatal("precondition failure ignored")
	}
}
