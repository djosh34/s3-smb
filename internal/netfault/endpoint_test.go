// SPDX-License-Identifier: AGPL-3.0-only

package netfault

import (
	"errors"
	"net"
	"strconv"
	"syscall"
	"testing"
	"time"
)

func TestResolvedPeer(t *testing.T) {
	peer := startEcho(t)
	address, ok := peer.listener.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatal("echo address is not TCP")
	}
	for _, host := range []string{"localhost", "127.0.0.1"} {
		t.Run(host, func(t *testing.T) {
			// These peers are local but have a different port from the proxy.
			upstream := net.JoinHostPort(host, "00"+strconv.Itoa(address.Port))
			proxy := newProxy(t.Context(), t, upstream)
			exchange(t, dialProxy(t, proxy))
		})
	}
}

func TestDialPeerTriesNextAddress(t *testing.T) {
	peer := startEcho(t)
	var config net.ListenConfig
	listener, err := config.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	refused := listener.Addr().String()
	if closeErr := listener.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	proxy := &Proxy{upstreams: []string{refused, peer.listener.Addr().String()}}
	conn, err := proxy.dialPeer(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeSocket(t, conn) })
	if err := conn.SetDeadline(time.Now().Add(testTimeout)); err != nil {
		t.Fatal(err)
	}
	exchange(t, conn)
}

func TestPeerRefusesConnections(t *testing.T) {
	var config net.ListenConfig
	listener, err := config.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := listener.Close(); closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
			t.Error(closeErr)
		}
	})
	// Construct the proxy while the target port is reserved. Closing the peer
	// afterward cannot let the proxy acquire that port and dial itself.
	proxy := newProxy(t.Context(), t, listener.Addr().String())
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	conn, dialErr := proxy.dialPeer(t.Context())
	if conn != nil {
		closeSocket(t, conn)
	}
	if !errors.Is(dialErr, syscall.ECONNREFUSED) {
		t.Fatalf("dial did not reach a refused connection: %v", dialErr)
	}
	// The forwarding path must close its client after that failed upstream dial.
	requireDisconnected(t, dialProxy(t, proxy))
}
