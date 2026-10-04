// SPDX-License-Identifier: AGPL-3.0-only
package netfault

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"testing"
	"time"
)

func awaitCut(t *testing.T, proxy *Proxy, direction Direction, threshold int64) Event {
	t.Helper()
	var total int64
	deadline := time.NewTimer(testTimeout)
	defer deadline.Stop()
	for {
		select {
		case event := <-proxy.Events():
			if event.Direction != direction {
				continue
			}
			total += event.Bytes
			if event.Cut {
				if total != threshold || event.Connection == 0 {
					t.Fatalf("cut event %+v after %d bytes, want %d", event, total, threshold)
				}
				return event
			}
		case <-deadline.C:
			t.Fatal("no byte-cut event")
			return Event{}
		}
	}
}

func TestByteCut(t *testing.T) {
	for _, direction := range []Direction{ClientToServer, ServerToClient} {
		for _, threshold := range []int64{1, 17, (32 << 10) + 17} {
			t.Run(fmt.Sprintf("direction=%d/bytes=%d", direction, threshold), func(t *testing.T) {
				peer := startEcho(t)
				proxy := newProxy(t.Context(), t, peer.listener.Addr().String())
				setFault(t, proxy, Fault{CutAfter: threshold, CutDirection: direction})
				conn := dialProxy(t, proxy)
				payload := bytes.Repeat([]byte{42}, 128<<10)
				if _, err := conn.Write(payload); err != nil {
					t.Logf("write interrupted by cut: %v", err)
				}
				awaitCut(t, proxy, direction, threshold)
				got, err := io.ReadAll(conn)
				var networkErr net.Error
				if errors.As(err, &networkErr) && networkErr.Timeout() {
					t.Fatalf("cut left client connected: %v", err)
				}
				if err != nil {
					t.Logf("read interrupted by cut: %v", err)
				}
				if direction == ServerToClient && !bytes.Equal(got, payload[:threshold]) {
					t.Fatalf("received %d bytes, want exactly %d unchanged bytes", len(got), threshold)
				}
				setFault(t, proxy, Fault{})
				exchange(t, dialProxy(t, proxy))
			})
		}
	}
}

func TestByteCutCountsEachConnection(t *testing.T) {
	peer := startEcho(t)
	proxy := newProxy(t.Context(), t, peer.listener.Addr().String())
	setFault(t, proxy, Fault{CutAfter: 10, CutDirection: ServerToClient})
	first, second := dialProxy(t, proxy), dialProxy(t, proxy)
	for _, conn := range []net.Conn{first, second} {
		writeBytes(t, conn, []byte("12345"))
		readBytes(t, conn, []byte("12345"))
	}
	writeBytes(t, first, []byte("67890extra"))
	readBytes(t, first, []byte("67890"))
	requireDisconnected(t, first)
	// Cutting the first link must not close the second or consume its budget.
	writeBytes(t, second, []byte("6789"))
	readBytes(t, second, []byte("6789"))
	writeBytes(t, second, []byte("0extra"))
	readBytes(t, second, []byte("0"))
	requireDisconnected(t, second)
	cuts := make(map[uint64]int64)
	totals := make(map[uint64]int64)
	for len(cuts) != 2 {
		select {
		case event := <-proxy.Events():
			if event.Direction == ServerToClient {
				totals[event.Connection] += event.Bytes
				if event.Cut {
					cuts[event.Connection] = totals[event.Connection]
				}
			}
		case <-time.After(testTimeout):
			t.Fatal("missing per-connection cut")
		}
	}
	for id, total := range cuts {
		if id == 0 || total != 10 {
			t.Fatalf("connection %d cut at %d bytes", id, total)
		}
	}
}

func TestInvalidByteCutKeepsFault(t *testing.T) {
	peer := startEcho(t)
	proxy := newProxy(t.Context(), t, peer.listener.Addr().String())
	setFault(t, proxy, Fault{Stall: true})
	for _, fault := range []Fault{{CutAfter: -1}, {CutAfter: 1}, {CutAfter: 1, CutDirection: 3}, {CutDirection: 3}} {
		if err := proxy.SetFault(fault); err == nil {
			t.Fatalf("accepted invalid fault %+v", fault)
		}
		if _, err := proxy.Schedule(t.Context(), []Step{{Fault: fault}}); err == nil {
			t.Fatalf("scheduled invalid fault %+v", fault)
		}
	}
	conn := dialProxy(t, proxy)
	writeBytes(t, conn, []byte("still stalled"))
	requireBlocked(t, conn)
	setFault(t, proxy, Fault{})
	readBytes(t, conn, []byte("still stalled"))
}

func TestForwardingEventsAndOverflow(t *testing.T) {
	peer := startEcho(t)
	proxy := newProxy(t.Context(), t, peer.listener.Addr().String())
	conn := dialProxy(t, proxy)
	const exchanges = 256
	for range exchanges {
		writeBytes(t, conn, []byte{42})
		readBytes(t, conn, []byte{42})
	}
	droppedBeforeCut := proxy.DroppedEvents()
	if err := proxy.Cut(); err != nil {
		t.Fatal(err)
	}
	if proxy.DroppedEvents() <= droppedBeforeCut {
		t.Fatal("overflow did not count the lost cut event")
	}
	if err := proxy.Close(); err != nil {
		t.Fatal(err)
	}
	var count uint64
	var id uint64
	for event := range proxy.Events() {
		if id == 0 {
			id = event.Connection
		}
		if event.Connection != id || event.Bytes != 1 || event.Cut || (event.Direction != ClientToServer && event.Direction != ServerToClient) {
			t.Fatalf("unexpected forwarding event %+v", event)
		}
		count++
	}
	if dropped := proxy.DroppedEvents(); dropped == 0 || count+dropped != 2*exchanges+1 {
		t.Fatalf("observed %d, dropped %d, want %d total events", count, dropped, 2*exchanges+1)
	}
	if err := proxy.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestManualCutEvents(t *testing.T) {
	for _, drop := range []bool{false, true} {
		t.Run(fmt.Sprint(drop), func(t *testing.T) {
			peer := startEcho(t)
			proxy := newProxy(t.Context(), t, peer.listener.Addr().String())
			conn := dialProxy(t, proxy)
			exchange(t, conn)
			if drop {
				setFault(t, proxy, Fault{Drop: true})
			} else if err := proxy.Cut(); err != nil {
				t.Fatal(err)
			}
			requireDisconnected(t, conn)
			deadline := time.NewTimer(testTimeout)
			defer deadline.Stop()
			for {
				select {
				case event := <-proxy.Events():
					if !event.Cut {
						continue
					}
					if event.Direction != 0 || event.Bytes != 0 || event.Connection == 0 {
						t.Fatalf("manual cut event %+v", event)
					}
					return
				case <-deadline.C:
					t.Fatal("no manual cut event")
				}
			}
		})
	}
}

type signaledConn struct {
	net.Conn
	started chan struct{}
}

func (c signaledConn) Write(data []byte) (int, error) {
	close(c.started)
	return c.Conn.Write(data)
}

func TestFaultReplacementDuringWrite(t *testing.T) {
	left, right := net.Pipe()
	t.Cleanup(func() { closeSocket(t, left); closeSocket(t, right) })
	for _, conn := range []net.Conn{left, right} {
		if err := conn.SetDeadline(time.Now().Add(testTimeout)); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	connection := &link{ctx: ctx, cancel: cancel, client: left, id: 1}
	proxy := &Proxy{links: map[*link]struct{}{connection: {}}, changed: make(chan struct{}), events: make(chan Event, 4)}
	setFault(t, proxy, Fault{CutAfter: 8, CutDirection: ClientToServer})
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- proxy.forwardBytes(connection, ClientToServer, signaledConn{left, started}, []byte("12345678extra"))
	}()
	select {
	case <-started:
	case <-time.After(testTimeout):
		t.Fatal("write did not start")
	}
	// Replacement must neither block on the old write nor inherit its count.
	setFault(t, proxy, Fault{CutAfter: 4, CutDirection: ClientToServer})
	readBytes(t, right, []byte("12345678"))
	if err := <-done; err != nil {
		t.Fatalf("stale cut fired: %v", err)
	}
	if event := <-proxy.Events(); event.Bytes != 8 || event.Cut {
		t.Fatalf("old write event %+v", event)
	}
	go func() { done <- proxy.forwardBytes(connection, ClientToServer, left, []byte("abcdef")) }()
	readBytes(t, right, []byte("abcd"))
	if err := <-done; !errors.Is(err, net.ErrClosed) {
		t.Fatalf("new cut returned %v", err)
	}
	if event := <-proxy.Events(); event.Bytes != 4 || !event.Cut {
		t.Fatalf("replacement cut event %+v", event)
	}
}
