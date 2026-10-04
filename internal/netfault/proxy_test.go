// SPDX-License-Identifier: AGPL-3.0-only

package netfault_test

import (
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/netfault"
)

// newProxy starts a proxy in front of an echo server.
func newProxy(t *testing.T) *netfault.Proxy {
	t.Helper()
	var config net.ListenConfig
	listener, err := config.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var echoes sync.WaitGroup
	echoes.Go(func() {
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				if !errors.Is(acceptErr, net.ErrClosed) {
					t.Error(acceptErr)
				}
				return
			}
			echoes.Go(func() {
				// The echo ends when the proxy closes its side.
				if _, copyErr := io.Copy(conn, conn); copyErr != nil {
					t.Logf("echo ended: %v", copyErr)
				}
				if closeErr := conn.Close(); closeErr != nil {
					t.Error(closeErr)
				}
			})
		}
	})
	proxy, err := netfault.New(t.Context(), listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := proxy.Close(); closeErr != nil {
			t.Error(closeErr)
		}
		if closeErr := listener.Close(); closeErr != nil {
			t.Error(closeErr)
		}
		echoes.Wait()
	})
	return proxy
}

func dial(t *testing.T, proxy *netfault.Proxy) net.Conn {
	t.Helper()
	conn, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", proxy.Address())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := conn.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	return conn
}

func echo(conn net.Conn) error {
	if _, err := conn.Write([]byte("ping")); err != nil {
		return err
	}
	reply := make([]byte, 4)
	if _, err := io.ReadFull(conn, reply); err != nil {
		return err
	}
	if string(reply) != "ping" {
		return errors.New("echo returned " + string(reply))
	}
	return nil
}

// assertClosed checks that the proxy closed conn: a read ends without data.
func assertClosed(t *testing.T, conn net.Conn) {
	t.Helper()
	n, err := conn.Read(make([]byte, 1))
	var timeout net.Error
	if n != 0 || err == nil || errors.As(err, &timeout) && timeout.Timeout() {
		t.Fatalf("connection still open: read %d bytes, error %v", n, err)
	}
}

func TestCut(t *testing.T) {
	proxy := newProxy(t)
	before := dial(t, proxy)
	if err := echo(before); err != nil {
		t.Fatal(err)
	}
	if err := proxy.Cut(); err != nil {
		t.Fatal(err)
	}
	assertClosed(t, before)
	if err := echo(dial(t, proxy)); err != nil {
		t.Fatalf("new connection after a cut: %v", err)
	}
}

func TestDropAndRestore(t *testing.T) {
	proxy := newProxy(t)
	before := dial(t, proxy)
	if err := echo(before); err != nil {
		t.Fatal(err)
	}
	if err := proxy.Drop(); err != nil {
		t.Fatal(err)
	}
	assertClosed(t, before)
	assertClosed(t, dial(t, proxy))
	proxy.Restore()
	if err := echo(dial(t, proxy)); err != nil {
		t.Fatalf("new connection after restore: %v", err)
	}
}
