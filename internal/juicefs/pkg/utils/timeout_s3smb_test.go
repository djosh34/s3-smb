/*
 * JuiceFS, Copyright 2021 Juicedata, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

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
