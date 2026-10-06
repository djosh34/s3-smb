// SPDX-License-Identifier: AGPL-3.0-only
package app

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/server"
	"github.com/djosh34/s3-smb/internal/smb/smbtest"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

// negotiateWithoutGCM sends one NEGOTIATE that offers no AES-GCM and returns
// the reply status.
func negotiateWithoutGCM(ctx context.Context, t *testing.T, address string) smb.Status {
	t.Helper()
	conn, err := new(net.Dialer).DialContext(ctx, "tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	client, err := smbtest.NewClient(conn)
	if err != nil {
		t.Fatal(errors.Join(err, conn.Close()))
	}
	defer func() {
		if e := client.Close(); e != nil {
			t.Error(e)
		}
	}()
	preauth, err := wire.EncodePreauthContext(wire.PreauthContext{Hashes: []uint16{smb.PreauthSHA512}})
	if err != nil {
		t.Fatal(err)
	}
	body, err := wire.EncodeNegotiateRequest(wire.NegotiateRequest{
		Dialects: []uint16{smb.Dialect311}, SecurityMode: smb.AdvertisedSecurityMode,
		Contexts: []wire.NegotiateContext{preauth},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = client.Send(ctx, []wire.Message{{Header: wire.Header{Command: wire.Negotiate, Credit: 1}, Body: body}}); err != nil {
		t.Fatal(err)
	}
	reply, err := client.Receive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(reply.Messages) != 1 {
		t.Fatalf("negotiation: %+v", reply.Messages)
	}
	return reply.Messages[0].Header.Status
}

// The configured encryption policy reaches the server: without GCM a client
// is refused only when encryption is required.
func TestSMBStartsAndStops(t *testing.T) {
	for _, encryption := range []bool{true, false} {
		t.Run(fmt.Sprintf("encryption=%t", encryption), func(t *testing.T) {
			r := serverResources(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			c := serverConfig()
			c.Encryption = encryption
			if err := r.startSMB(ctx, c); err != nil {
				t.Fatal(err)
			}
			if _, ok := r.server.(*server.Server); !ok {
				t.Fatalf("server type %T", r.server)
			}
			want := smb.StatusSuccess
			if encryption {
				want = smb.StatusNotSupported
			}
			if status := negotiateWithoutGCM(ctx, t, r.listener.Addr().String()); status != want {
				t.Fatalf("negotiation status %v, want %v", status, want)
			}
			cancel()
			if err := <-r.serveDone; !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled Serve: %v", err)
			}
			if conn, err := new(net.Dialer).DialContext(t.Context(), "tcp", r.listener.Addr().String()); err == nil {
				t.Fatal(errors.Join(errors.New("listener remained open"), conn.Close()))
			}
		})
	}
}

// A rejected configuration leaves nothing open.
func TestSMBConstructorFailureLeavesNothingOpen(t *testing.T) {
	r := serverResources(t)
	c := serverConfig()
	c.Share = "IPC$"
	if err := r.startSMB(t.Context(), c); err == nil {
		t.Fatal("accepted an invalid share name")
	}
	if r.server != nil || r.listener != nil {
		t.Fatal("constructor returned resources on failure")
	}
}
