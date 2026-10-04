// SPDX-License-Identifier: AGPL-3.0-only
package helpers

import (
	"context"
	"strings"
	"time"
)

// RetryDownload makes up to three attempts on transient network failures.
// Each failure, including the last, is reported before waiting or returning.
func RetryDownload(ctx context.Context, delay time.Duration, run func() (string, error), failed func(int, string, error)) error {
	for attempt := 1; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		output, err := run()
		if err == nil {
			return nil
		}
		failed(attempt, output, err)
		if attempt == 3 || !transientNetworkFailure(output) {
			return err
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func transientNetworkFailure(output string) bool {
	output = strings.ToLower(output)
	for _, message := range []string{
		"could not resolve host", "could not resolve proxy", "no such host",
		"temporary failure in name resolution", "connection reset", "connection refused",
		"network is unreachable", "i/o timeout", "tls handshake timeout",
		"connection timed out", "operation timed out", "unexpected eof",
		"failed to connect to", "couldn't connect to server", "could not connect to server",
		"rpc failed; curl 18", "rpc failed; curl 28", "rpc failed; curl 52",
		"rpc failed; curl 55", "rpc failed; curl 56", "early eof",
		"unexpected disconnect while reading sideband packet",
		"500 internal server error", "502 bad gateway", "503 service unavailable", "504 gateway timeout",
		"the requested url returned error: 500", "the requested url returned error: 502",
		"the requested url returned error: 503", "the requested url returned error: 504",
	} {
		if strings.Contains(output, message) {
			return true
		}
	}
	return false
}
