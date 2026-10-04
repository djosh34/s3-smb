// SPDX-License-Identifier: AGPL-3.0-only

package netfault

import (
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

// echoTraffic keeps normal request/response I/O running until a cut interrupts
// it. The first reply proves forwarding started before the test arms the cut.
func echoTraffic(conn net.Conn, started chan<- struct{}) error {
	first := true
	payload := []byte{42}
	for {
		n, err := conn.Write(payload)
		if err != nil {
			return err
		}
		if n != len(payload) {
			return io.ErrShortWrite
		}
		var reply [1]byte
		if _, err := io.ReadFull(conn, reply[:]); err != nil {
			return err
		}
		if reply[0] != payload[0] {
			return errors.New("echo traffic changed")
		}
		if first {
			close(started)
			first = false
		}
	}
}

func TestCutDuringNormalTraffic(t *testing.T) {
	for _, name := range []string{"command", "schedule"} {
		t.Run(name, func(t *testing.T) {
			peer := startEcho(t)
			proxy := newProxy(t.Context(), t, peer.listener.Addr().String())
			conn := dialProxy(t, proxy)
			started := make(chan struct{})
			finished := make(chan error, 1)
			go func() { finished <- echoTraffic(conn, started) }()
			select {
			case <-started:
			case err := <-finished:
				t.Fatalf("traffic ended before cut: %v", err)
			case <-time.After(testTimeout):
				t.Fatal("traffic did not start")
			}
			if name == "command" {
				if err := proxy.Cut(); err != nil {
					t.Fatal(err)
				}
			} else {
				done, err := proxy.Schedule(t.Context(), []Step{{After: 10 * time.Millisecond, Cut: true}})
				if err != nil {
					t.Fatal(err)
				}
				awaitSchedule(t, done, nil)
			}
			select {
			case err := <-finished:
				var networkErr net.Error
				if !errors.Is(err, io.EOF) && (!errors.As(err, &networkErr) || networkErr.Timeout()) {
					t.Fatalf("traffic did not end with a disconnect: %v", err)
				}
			case <-time.After(testTimeout):
				t.Fatal("cut did not interrupt traffic")
			}
			exchange(t, dialProxy(t, proxy))
		})
	}
}

func TestManualCommandKeepsSchedule(t *testing.T) {
	peer := startEcho(t)
	proxy := newProxy(t.Context(), t, peer.listener.Addr().String())
	setFault(t, proxy, Fault{Drop: true})
	// A manual cut takes effect now, but the scheduled restore still runs.
	done, err := proxy.Schedule(t.Context(), []Step{{After: 100 * time.Millisecond}})
	if err != nil {
		t.Fatal(err)
	}
	if err := proxy.Cut(); err != nil {
		t.Fatal(err)
	}
	awaitSchedule(t, done, nil)
	exchange(t, dialProxy(t, proxy))
}
