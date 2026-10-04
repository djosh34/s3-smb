// SPDX-License-Identifier: AGPL-3.0-only
package helpers

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"testing"
	"time"
)

func TestRetryDownload(t *testing.T) {
	for _, failures := range []int{0, 1, 2, 3} {
		t.Run(strconv.Itoa(failures)+" failures", func(t *testing.T) {
			calls := 0
			var logs []int
			var lastErr error
			err := RetryDownload(t.Context(), time.Millisecond, func() (string, error) {
				calls++
				if calls > failures {
					return "done", nil
				}
				lastErr = errors.New("fetch failed")
				return "fatal: Could not resolve host: github.com", lastErr
			}, func(attempt int, output string, err error) {
				logs = append(logs, attempt)
				if output != "fatal: Could not resolve host: github.com" || !errors.Is(err, lastErr) {
					t.Fatal("lost failed attempt output or error", output, err)
				}
			})
			if calls != min(failures+1, 3) || !slices.Equal(logs, []int{1, 2, 3}[:failures]) {
				t.Fatal("wrong attempts or failure logs", calls, logs)
			}
			if failures == 3 {
				if !errors.Is(err, lastErr) {
					t.Fatal("did not return the last error", err)
				}
			} else if err != nil {
				t.Fatal("did not stop on success", err)
			}
		})
	}
}

func TestRetryDownloadPermanentFailure(t *testing.T) {
	for _, output := range []string{
		"", "fatal: couldn't find remote ref bad-revision", "fatal: Authentication failed",
		"go: invalid go.mod", "undefined: missingSymbol", "go: checksum mismatch",
		"x509: certificate signed by unknown authority", "404 Not Found",
	} {
		t.Run(output, func(t *testing.T) {
			failure := errors.New("permanent failure")
			calls, logs := 0, 0
			err := RetryDownload(t.Context(), time.Hour, func() (string, error) {
				calls++
				return output, failure
			}, func(attempt int, text string, err error) {
				logs++
				if attempt != 1 || text != output || !errors.Is(err, failure) {
					t.Fatal("wrong failure log", attempt, text, err)
				}
			})
			if !errors.Is(err, failure) || calls != 1 || logs != 1 {
				t.Fatal("retried a permanent failure", calls, logs, err)
			}
		})
	}
}

func TestRetryDownloadCancellation(t *testing.T) {
	for _, before := range []bool{true, false} {
		t.Run(map[bool]string{true: "before attempt", false: "during wait"}[before], func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if before {
				cancel()
			}
			calls, logs := 0, 0
			err := RetryDownload(ctx, time.Hour, func() (string, error) {
				calls++
				return "connection reset", errors.New("fetch failed")
			}, func(int, string, error) {
				logs++
				cancel()
			})
			want := 1
			if before {
				want = 0
			}
			if !errors.Is(err, context.Canceled) || calls != want || logs != want {
				t.Fatal("did not stop on cancellation", calls, logs, err)
			}
		})
	}
}

func TestTransientNetworkFailure(t *testing.T) {
	for _, output := range []string{
		"fatal: Could not resolve host: github.com", "Could not resolve proxy: proxy.example",
		"dial tcp: lookup proxy.golang.org: no such host", "Temporary failure in name resolution",
		"read: connection reset by peer", "connect: connection refused", "network is unreachable",
		"dial tcp: i/o timeout", "net/http: TLS handshake timeout", "Connection timed out",
		"Operation timed out", "unexpected EOF", "500 Internal Server Error", "502 Bad Gateway",
		"503 Service Unavailable", "504 Gateway Timeout",
		"fatal: The requested URL returned error: 500", "fatal: The requested URL returned error: 502",
		"fatal: The requested URL returned error: 503", "fatal: The requested URL returned error: 504",
	} {
		if !transientNetworkFailure(output) {
			t.Error("did not recognize a transient network error", output)
		}
	}
}

func TestRetryDownloadWait(t *testing.T) {
	const delay = 10 * time.Millisecond
	calls := 0
	start := time.Now()
	must(t, RetryDownload(t.Context(), delay, func() (string, error) {
		calls++
		if calls == 1 {
			return "unexpected EOF", errors.New("download interrupted")
		}
		if time.Since(start) < delay {
			t.Error("retried before the wait ended")
		}
		return "done", nil
	}, func(int, string, error) {}))
	if calls != 2 {
		t.Fatal("wrong attempt count", calls)
	}
}
