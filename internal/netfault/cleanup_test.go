// SPDX-License-Identifier: AGPL-3.0-only

package netfault

import (
	"context"
	"errors"
	"net"
	"testing"
)

type failingCloseConn struct {
	net.Conn
	closeErr error
}

func (conn failingCloseConn) Close() error {
	return errors.Join(conn.Conn.Close(), conn.closeErr)
}

func TestCommandCleanupErrors(t *testing.T) {
	for _, command := range []string{"SetFault", "Cut"} {
		t.Run(command, func(t *testing.T) {
			peer := startEcho(t)
			proxy, err := New(t.Context(), peer.listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			earlier := errors.New("earlier cleanup failure")
			current := errors.New("current cleanup failure")
			t.Cleanup(func() {
				if closeErr := proxy.Close(); !errors.Is(closeErr, earlier) || !errors.Is(closeErr, current) {
					t.Errorf("Close did not collect cleanup errors: %v", closeErr)
				}
			})
			client, remote := net.Pipe()
			t.Cleanup(func() { closeSocket(t, remote) })
			ctx, cancel := context.WithCancel(proxy.ctx)
			connection := &link{ctx: ctx, cancel: cancel, client: failingCloseConn{Conn: client, closeErr: current}}
			proxy.mu.Lock()
			proxy.recordClose(earlier)
			proxy.links[connection] = struct{}{}
			proxy.mu.Unlock()
			if setErr := proxy.SetFault(Fault{Stall: true}); setErr != nil {
				t.Fatalf("SetFault reported an earlier error: %v", setErr)
			}
			var commandErr error
			if command == "SetFault" {
				commandErr = proxy.SetFault(Fault{Drop: true})
			} else {
				commandErr = proxy.Cut()
			}
			if !errors.Is(commandErr, current) || errors.Is(commandErr, earlier) {
				t.Fatalf("%s did not report only its own error: %v", command, commandErr)
			}
			setFault(t, proxy, Fault{})
			if cutErr := proxy.Cut(); cutErr != nil {
				t.Fatalf("Cut reported an earlier error: %v", cutErr)
			}
		})
	}
}
