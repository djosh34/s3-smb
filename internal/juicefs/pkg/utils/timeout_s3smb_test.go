// SPDX-License-Identifier: AGPL-3.0-only
// Modified for s3-smb, 2026. See docs/vendored.md.

package utils

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Adapted from juicedata/juicefs PR #7503. The callback returns after cancellation.
func TestWithTimeoutCancelResultRace(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	release := make(chan struct{})
	callbackReturning := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		result <- WithTimeout(ctx, func(ctx context.Context) error {
			close(started)
			<-ctx.Done()
			<-release
			// Do not synchronize the late write with WithTimeout's return.
			time.Sleep(10 * time.Millisecond)
			close(callbackReturning)
			return nil
		}, time.Second)
	}()

	<-started
	cancel()
	close(release)
	err := <-result
	<-callbackReturning
	// Allow the worker to publish its result before the test ends.
	time.Sleep(time.Millisecond)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want cancellation, got %v", err)
	}
}

func TestWithTimeoutResults(t *testing.T) {
	want := errors.New("callback failed")
	for _, callbackErr := range []error{nil, want} {
		err := WithTimeout(context.Background(), func(context.Context) error {
			return callbackErr
		}, time.Second)
		if !errors.Is(err, callbackErr) {
			t.Fatalf("want %v, got %v", callbackErr, err)
		}
	}

	returning := make(chan struct{})
	err := WithTimeout(context.Background(), func(ctx context.Context) error {
		<-ctx.Done()
		time.Sleep(10 * time.Millisecond)
		close(returning)
		return want
	}, time.Millisecond)
	<-returning
	time.Sleep(time.Millisecond)
	if !errors.Is(err, ErrFuncTimeout) {
		t.Fatalf("want timeout, got %v", err)
	}
}
