// SPDX-License-Identifier: AGPL-3.0-only

package netfault

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

const testTimeout = 3 * time.Second

// echoPeer keeps every socket owned until cleanup, including stalled clients.
type echoPeer struct {
	listener net.Listener
	clients  map[net.Conn]struct{}
	mu       sync.Mutex
	workers  sync.WaitGroup
	accepted int
	closed   bool
}

func startEcho(t *testing.T) *echoPeer {
	t.Helper()
	var config net.ListenConfig
	listener, err := config.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	peer := &echoPeer{listener: listener, clients: make(map[net.Conn]struct{})}
	peer.workers.Add(1)
	go func() {
		defer peer.workers.Done()
		for {
			client, acceptErr := listener.Accept()
			if acceptErr != nil {
				if !errors.Is(acceptErr, net.ErrClosed) {
					t.Errorf("accept echo client: %v", acceptErr)
				}
				return
			}
			peer.mu.Lock()
			if peer.closed {
				closeSocket(t, client)
				peer.mu.Unlock()
				continue
			}
			peer.clients[client] = struct{}{}
			peer.accepted++
			peer.workers.Add(1)
			peer.mu.Unlock()
			go func() {
				defer peer.workers.Done()
				if _, copyErr := io.Copy(client, client); copyErr != nil {
					// Faults deliberately interrupt the echo peer's I/O.
					t.Logf("echo connection ended: %v", copyErr)
				}
				peer.mu.Lock()
				if _, owned := peer.clients[client]; owned {
					closeSocket(t, client)
					delete(peer.clients, client)
				}
				peer.mu.Unlock()
			}()
		}
	}()
	t.Cleanup(func() {
		peer.mu.Lock()
		peer.closed = true
		if err := listener.Close(); err != nil {
			t.Error(err)
		}
		for client := range peer.clients {
			closeSocket(t, client)
			delete(peer.clients, client)
		}
		peer.mu.Unlock()
		peer.workers.Wait()
	})
	return peer
}

func newProxy(ctx context.Context, t *testing.T, upstream string) *Proxy {
	t.Helper()
	proxy, err := New(ctx, upstream)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := proxy.Close(); err != nil {
			t.Error(err)
		}
	})
	return proxy
}

func dialProxy(t *testing.T, proxy *Proxy) net.Conn {
	t.Helper()
	var dialer net.Dialer
	conn, err := dialer.DialContext(t.Context(), "tcp", proxy.Address())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeSocket(t, conn) })
	if err := conn.SetDeadline(time.Now().Add(testTimeout)); err != nil {
		t.Fatal(err)
	}
	return conn
}

func closeSocket(t *testing.T, conn net.Conn) {
	t.Helper()
	if err := conn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		t.Error(err)
	}
}

func setFault(t *testing.T, proxy *Proxy, fault Fault) {
	t.Helper()
	if err := proxy.SetFault(fault); err != nil {
		t.Fatal(err)
	}
}

func writeBytes(t *testing.T, conn net.Conn, payload []byte) {
	t.Helper()
	n, err := conn.Write(payload)
	if err != nil {
		t.Fatal(err)
	}
	if n != len(payload) {
		t.Fatal("short client write")
	}
}

func readBytes(t *testing.T, conn net.Conn, payload []byte) {
	t.Helper()
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("echo mismatch: got %d bytes, want %d", len(got), len(payload))
	}
}

func exchange(t *testing.T, conn net.Conn) {
	t.Helper()
	payload := []byte("echo through a fault proxy")
	writeBytes(t, conn, payload)
	readBytes(t, conn, payload)
}

func requireBlocked(t *testing.T, conn net.Conn) {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(50 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	var probe [1]byte
	n, err := conn.Read(probe[:])
	var networkErr net.Error
	if n != 0 || !errors.As(err, &networkErr) || !networkErr.Timeout() {
		t.Fatalf("read while blocked: n=%d err=%v", n, err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(testTimeout)); err != nil {
		t.Fatal(err)
	}
}

func requireDisconnected(t *testing.T, conn net.Conn) {
	t.Helper()
	var probe [1]byte
	n, err := conn.Read(probe[:])
	var networkErr net.Error
	if n != 0 || err == nil || (errors.As(err, &networkErr) && networkErr.Timeout()) {
		t.Fatalf("expected disconnect, got n=%d err=%v", n, err)
	}
}

func TestForwardAndHalfClose(t *testing.T) {
	peer := startEcho(t)
	proxy := newProxy(t.Context(), t, peer.listener.Addr().String())
	for range 2 {
		conn := dialProxy(t, proxy)
		exchange(t, conn)
		payload := bytes.Repeat([]byte("long echo payload\x00"), 8192)
		writeBytes(t, conn, payload)
		tcp, ok := conn.(*net.TCPConn)
		if !ok {
			t.Fatal("client is not TCP")
		}
		if err := tcp.CloseWrite(); err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(conn)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, payload) {
			t.Fatal("half-close lost or changed data")
		}
	}
}

func TestCutDuringIO(t *testing.T) {
	peer := startEcho(t)
	proxy := newProxy(t.Context(), t, peer.listener.Addr().String())
	clients := []net.Conn{dialProxy(t, proxy), dialProxy(t, proxy)}
	for _, conn := range clients {
		exchange(t, conn)
	}
	setFault(t, proxy, Fault{Stall: true})
	for _, conn := range clients {
		writeBytes(t, conn, []byte("in-flight write"))
	}
	if err := proxy.Cut(); err != nil {
		t.Fatal(err)
	}
	for _, conn := range clients {
		requireDisconnected(t, conn)
	}
	setFault(t, proxy, Fault{})
	exchange(t, dialProxy(t, proxy))
}

func TestDelayAndReplacement(t *testing.T) {
	peer := startEcho(t)
	proxy := newProxy(t.Context(), t, peer.listener.Addr().String())
	conn := dialProxy(t, proxy)
	exchange(t, conn)
	const delay = 70 * time.Millisecond
	setFault(t, proxy, Fault{Delay: delay})
	start := time.Now()
	exchange(t, conn)
	if elapsed := time.Since(start); elapsed < 2*delay {
		t.Fatalf("round trip delay = %v, want at least %v", elapsed, 2*delay)
	}
	setFault(t, proxy, Fault{Delay: time.Hour})
	payload := []byte("preserve delayed bytes")
	writeBytes(t, conn, payload)
	requireBlocked(t, conn)
	setFault(t, proxy, Fault{})
	readBytes(t, conn, payload)
}

func TestStallAndResume(t *testing.T) {
	peer := startEcho(t)
	proxy := newProxy(t.Context(), t, peer.listener.Addr().String())
	conn := dialProxy(t, proxy)
	exchange(t, conn)
	setFault(t, proxy, Fault{Stall: true})
	payload := []byte("preserve stalled bytes")
	writeBytes(t, conn, payload)
	requireBlocked(t, conn)
	setFault(t, proxy, Fault{})
	readBytes(t, conn, payload)
	// Connections established while stalled resume too.
	setFault(t, proxy, Fault{Stall: true})
	second := dialProxy(t, proxy)
	writeBytes(t, second, payload)
	requireBlocked(t, second)
	setFault(t, proxy, Fault{})
	readBytes(t, second, payload)
	exchange(t, conn)
}

func TestDropAndRestore(t *testing.T) {
	peer := startEcho(t)
	proxy := newProxy(t.Context(), t, peer.listener.Addr().String())
	conn := dialProxy(t, proxy)
	exchange(t, conn)
	setFault(t, proxy, Fault{Drop: true})
	requireDisconnected(t, conn)
	for range 2 {
		requireDisconnected(t, dialProxy(t, proxy))
	}
	peer.mu.Lock()
	count := peer.accepted
	peer.mu.Unlock()
	if count != 1 {
		t.Fatalf("drop dialed additional peer connections: %d", count)
	}
	setFault(t, proxy, Fault{})
	exchange(t, dialProxy(t, proxy))
}

func TestCloseAndCancellation(t *testing.T) {
	for _, fault := range []Fault{{}, {Delay: time.Hour}, {Stall: true}} {
		for _, cancelParent := range []bool{false, true} {
			t.Run(testName(fault, cancelParent), func(t *testing.T) {
				peer := startEcho(t)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				proxy := newProxy(ctx, t, peer.listener.Addr().String())
				conn := dialProxy(t, proxy)
				exchange(t, conn)
				setFault(t, proxy, fault)
				if fault.Delay != 0 || fault.Stall {
					writeBytes(t, conn, []byte("pending"))
					requireBlocked(t, conn)
				}
				if cancelParent {
					cancel()
				} else if err := proxy.Close(); err != nil {
					t.Fatal(err)
				}
				requireDisconnected(t, conn)
				closed := make(chan error, 1)
				go func() { closed <- proxy.Close() }()
				select {
				case err := <-closed:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(testTimeout):
					t.Fatal("Close did not drain workers")
				}
				if err := proxy.SetFault(Fault{}); !errors.Is(err, net.ErrClosed) {
					t.Fatalf("SetFault after Close: %v", err)
				}
				if err := proxy.Cut(); !errors.Is(err, net.ErrClosed) {
					t.Fatalf("Cut after Close: %v", err)
				}
			})
		}
	}
}

func testName(fault Fault, canceled bool) string {
	name := "idle"
	if fault.Delay != 0 {
		name = "delay"
	} else if fault.Stall {
		name = "stall"
	}
	if canceled {
		return name + "/cancel"
	}
	return name + "/close"
}

func TestInvalidInputs(t *testing.T) {
	for _, upstream := range []string{"", "http://127.0.0.1:123", ":123", "localhost:0", "localhost:65536", "localhost:abc"} {
		if proxy, err := New(t.Context(), upstream); err == nil {
			if closeErr := proxy.Close(); closeErr != nil {
				t.Error(closeErr)
			}
			t.Errorf("accepted invalid upstream %q", upstream)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if proxy, err := New(ctx, "127.0.0.1:123"); !errors.Is(err, context.Canceled) {
		if proxy != nil {
			if closeErr := proxy.Close(); closeErr != nil {
				t.Error(closeErr)
			}
		}
		t.Fatalf("canceled New: %v", err)
	}
	peer := startEcho(t)
	proxy := newProxy(t.Context(), t, peer.listener.Addr().String())
	setFault(t, proxy, Fault{Stall: true})
	if err := proxy.SetFault(Fault{Delay: -1}); err == nil {
		t.Fatal("accepted negative delay")
	}
	conn := dialProxy(t, proxy)
	payload := []byte("invalid replacement keeps stall")
	writeBytes(t, conn, payload)
	requireBlocked(t, conn)
	setFault(t, proxy, Fault{})
	readBytes(t, conn, payload)
}

func TestUnavailablePeer(t *testing.T) {
	var config net.ListenConfig
	listener, err := config.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	proxy := newProxy(t.Context(), t, address)
	requireDisconnected(t, dialProxy(t, proxy))
}
