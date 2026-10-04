// SPDX-License-Identifier: AGPL-3.0-only

package netfault

import (
	"bytes"
	"context"
	"errors"
	"math"
	"testing"
	"time"
)

func TestBandwidthCap(t *testing.T) {
	peer := startEcho(t)
	proxy := newProxy(t.Context(), t, peer.listener.Addr().String())
	conn := dialProxy(t, proxy)
	exchange(t, conn)
	const rate = 128 * 1024
	setFault(t, proxy, Fault{BytesPerSecond: rate})
	payload := make([]byte, 64*1024)
	for i := range payload {
		payload[i] = byte(i*31 + i/257)
	}
	started := time.Now()
	writeBytes(t, conn, payload)
	readBytes(t, conn, payload)
	// Forwarding can pipeline the two directions, but each must charge all
	// bytes. The round trip cannot be faster than one direction's cap.
	minimum := time.Duration(int64(len(payload)) * int64(time.Second) / rate)
	if elapsed := time.Since(started); elapsed < minimum {
		t.Fatalf("cap forwarded %d bytes in %s, want at least %s", len(payload), elapsed, minimum)
	}
}

func TestBandwidthRestorePreservesBytes(t *testing.T) {
	for _, replacement := range []Fault{{}, {BytesPerSecond: 1024 * 1024}, {Stall: true}} {
		t.Run(testName(replacement, false), func(t *testing.T) {
			peer := startEcho(t)
			proxy := newProxy(t.Context(), t, peer.listener.Addr().String())
			conn := dialProxy(t, proxy)
			exchange(t, conn)
			setFault(t, proxy, Fault{BytesPerSecond: 1, CutAfter: 7, CutDirection: ClientToServer})
			payload := bytes.Repeat([]byte("unchanged buffered suffix\x00"), 4096)
			writeBytes(t, conn, payload)
			requireBlocked(t, conn)
			setFault(t, proxy, replacement)
			if replacement.Stall {
				requireBlocked(t, conn)
				setFault(t, proxy, Fault{})
			}
			readBytes(t, conn, payload)
			exchange(t, conn)
		})
	}
}

func TestBandwidthCancelAndClose(t *testing.T) {
	for _, cancelParent := range []bool{false, true} {
		t.Run(testName(Fault{}, cancelParent), func(t *testing.T) {
			peer := startEcho(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			proxy := newProxy(ctx, t, peer.listener.Addr().String())
			conn := dialProxy(t, proxy)
			exchange(t, conn)
			setFault(t, proxy, Fault{BytesPerSecond: 1})
			writeBytes(t, conn, []byte("pending bandwidth wait"))
			requireBlocked(t, conn)
			if cancelParent {
				cancel()
			}
			closed := make(chan error, 1)
			go func() { closed <- proxy.Close() }()
			select {
			case err := <-closed:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(testTimeout):
				t.Fatal("bandwidth wait blocked shutdown")
			}
			requireDisconnected(t, conn)
		})
	}
}

func TestBandwidthConnectionsHaveSeparateBudgets(t *testing.T) {
	peer := startEcho(t)
	proxy := newProxy(t.Context(), t, peer.listener.Addr().String())
	first, second := dialProxy(t, proxy), dialProxy(t, proxy)
	exchange(t, first)
	exchange(t, second)
	setFault(t, proxy, Fault{BytesPerSecond: 1})
	writeBytes(t, first, make([]byte, 32*1024))
	requireBlocked(t, first)
	// The first connection has hours of queued transfer time. The second
	// owes just a second per direction, not any of the first one's time.
	writeBytes(t, second, []byte{42})
	readBytes(t, second, []byte{42})
}

func TestBandwidthDirectionsHaveSeparateBudgets(t *testing.T) {
	peer := startEcho(t)
	proxy := newProxy(t.Context(), t, peer.listener.Addr().String())
	conn := dialProxy(t, proxy)
	exchange(t, conn)
	for len(proxy.events) > 0 {
		<-proxy.Events()
	}
	const rate = 32 * 1024
	setFault(t, proxy, Fault{BytesPerSecond: rate})
	payload := bytes.Repeat([]byte{42}, rate)
	writeBytes(t, conn, payload)
	var sent int64
	for sent < int64(len(payload)) {
		event := bandwidthEvent(t, proxy)
		if event.Direction == ClientToServer {
			sent += event.Bytes
		}
	}
	// The echo is now being paced in the opposite direction. That debt must
	// not stop this direction's tiny follow-up request.
	writeBytes(t, conn, []byte{43})
	deadline := time.NewTimer(500 * time.Millisecond)
	defer deadline.Stop()
	for {
		select {
		case event := <-proxy.Events():
			if event.Direction == ClientToServer && event.Bytes == 1 {
				readBytes(t, conn, append(payload, 43))
				return
			}
		case <-deadline.C:
			t.Fatal("opposite-direction bandwidth wait held this direction")
		}
	}
}

func bandwidthEvent(t *testing.T, proxy *Proxy) Event {
	t.Helper()
	select {
	case event := <-proxy.Events():
		return event
	case <-time.After(testTimeout):
		t.Fatal("no forwarded byte event")
		return Event{}
	}
}

func TestBandwidthRejectsNegativeRate(t *testing.T) {
	peer := startEcho(t)
	proxy := newProxy(t.Context(), t, peer.listener.Addr().String())
	setFault(t, proxy, Fault{Stall: true})
	if err := proxy.SetFault(Fault{BytesPerSecond: -1}); err == nil {
		t.Fatal("accepted a negative bandwidth cap")
	}
	if _, err := proxy.Schedule(t.Context(), []Step{{Fault: Fault{BytesPerSecond: -1}}}); err == nil {
		t.Fatal("scheduled a negative bandwidth cap")
	}
	conn := dialProxy(t, proxy)
	payload := []byte("invalid replacement must keep stall")
	writeBytes(t, conn, payload)
	requireBlocked(t, conn)
	setFault(t, proxy, Fault{})
	readBytes(t, conn, payload)
}

func TestBandwidthWait(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if ready, err := waitBandwidth(ctx, make(chan struct{}), 32*1024, 1); ready || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled wait: ready=%t error=%v", ready, err)
	}
	changed := make(chan struct{})
	close(changed)
	if ready, err := waitBandwidth(t.Context(), changed, 32*1024, 1); ready || err != nil {
		t.Fatalf("replaced wait: ready=%t error=%v", ready, err)
	}
	for _, rate := range []int64{0, math.MaxInt64} {
		if ready, err := waitBandwidth(t.Context(), make(chan struct{}), 32*1024, rate); !ready || err != nil {
			t.Fatalf("unlimited or high cap: ready=%t error=%v", ready, err)
		}
	}
}
