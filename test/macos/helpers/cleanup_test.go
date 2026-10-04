// SPDX-License-Identifier: AGPL-3.0-only
package helpers

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestCleanupReservesUnloadContext(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	var calls []string
	unloadErr := errors.New("unload failed")
	err := Cleanup(ctx, 0, func(clientCtx context.Context) error {
		calls = append(calls, "clients")
		return clientCtx.Err()
	}, func(unloadCtx context.Context) error {
		calls = append(calls, "unload")
		if unloadCtx != ctx || unloadCtx.Err() != nil {
			t.Fatal("client timeout consumed the unload context")
		}
		return unloadErr
	})
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, unloadErr) || !reflect.DeepEqual(calls, []string{"clients", "unload"}) {
		t.Fatal("cleanup lost an error or did not unload after client timeout", err, calls)
	}
}

func TestCleanupCancelsClientContext(t *testing.T) {
	for _, clientErr := range []error{nil, errors.New("detach failed")} {
		var child context.Context
		err := Cleanup(t.Context(), time.Minute, func(ctx context.Context) error {
			child = ctx
			return clientErr
		}, func(context.Context) error {
			if !errors.Is(child.Err(), context.Canceled) {
				t.Fatal("client context must be cancelled before unloading")
			}
			return nil
		})
		if !errors.Is(err, clientErr) {
			t.Fatal("cleanup lost the client error", err)
		}
	}
}
