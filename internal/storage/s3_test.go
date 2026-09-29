// SPDX-License-Identifier: AGPL-3.0-only
package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/djosh34/s3-smb/internal/juicefs/pkg/object"
)

func TestS3MissingIsNotAuthenticationFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		if strings.HasSuffix(r.URL.Path, "missing") {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, "<Error><Code>NoSuchKey</Code><Message>Missing</Message></Error>")
		} else {
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, "<Error><Code>AccessDenied</Code><Message>Denied</Message></Error>")
		}
	}))
	defer srv.Close()
	raw, err := object.NewS3(object.S3Options{Bucket: "fixture", Endpoint: srv.URL, AccessKey: "synthetic", SecretKey: "synthetic-secret"})
	if err != nil {
		t.Fatal(err)
	}
	defer raw.(io.Closer).Close()
	if _, err = raw.Get(context.Background(), "missing", 0, -1); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing: %v", err)
	}
	if _, err = raw.Get(context.Background(), "denied", 0, -1); err == nil || errors.Is(err, os.ErrNotExist) {
		t.Fatalf("auth mistaken for empty dataset: %v", err)
	}
}
func TestS3ConditionalPublicationHeader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.Header.Get("If-None-Match") != "*" {
			t.Errorf("unsafe publication: %s %q", r.Method, r.Header.Get("If-None-Match"))
		}
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusPreconditionFailed)
		fmt.Fprint(w, "<Error><Code>PreconditionFailed</Code><Message>Exists</Message></Error>")
	}))
	defer srv.Close()
	raw, err := object.NewS3(object.S3Options{Bucket: "fixture", Endpoint: srv.URL, AccessKey: "synthetic", SecretKey: "synthetic-secret"})
	if err != nil {
		t.Fatal(err)
	}
	defer raw.(io.Closer).Close()
	conditional := raw.(interface {
		PutIfAbsent(context.Context, string, io.Reader) error
	})
	if err = conditional.PutIfAbsent(context.Background(), "keep", strings.NewReader("data")); err == nil {
		t.Fatal("precondition failure ignored")
	}
}
