//go:build smbnext

// SPDX-License-Identifier: AGPL-3.0-only
package app

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/config"
	"github.com/djosh34/s3-smb/internal/juicefs/pkg/meta"
	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/auth"
	"github.com/djosh34/s3-smb/internal/smb/server"
	"github.com/djosh34/s3-smb/internal/smb/smbtest"
	"github.com/djosh34/s3-smb/internal/smb/state"
	"github.com/djosh34/s3-smb/internal/smb/wire"
	"github.com/djosh34/s3-smb/internal/smbfs"
)

func TestSMBNextStartsAndStops(t *testing.T) {
	for _, encryption := range []bool{true, false} {
		name := "plaintext allowed"
		if encryption {
			name = "encryption required"
		}
		t.Run(name, func(t *testing.T) {
			r, path := serverResources(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			c := serverConfig()
			c.Encryption = encryption
			if err := r.startSMB(ctx, c, path); err != nil {
				t.Fatal(err)
			}
			if _, ok := r.server.(*server.Server); !ok {
				t.Fatalf("smbnext server type %T", r.server)
			}
			conn, err := net.DialTimeout("tcp", r.listener.Addr().String(), 3*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			client, err := smbtest.NewClient(conn)
			if err != nil {
				t.Fatal(errors.Join(err, conn.Close()))
			}
			defer func() {
				if err := client.Close(); err != nil {
					t.Error(err)
				}
			}()
			requestCtx, stop := context.WithTimeout(ctx, 3*time.Second)
			defer stop()
			preauth, err := wire.EncodePreauthContext(wire.PreauthContext{Hashes: []uint16{smb.PreauthSHA512}})
			if err != nil {
				t.Fatal(err)
			}
			// Offer no GCM so the two policies have different wire results.
			body, err := wire.EncodeNegotiateRequest(wire.NegotiateRequest{
				Dialects: []uint16{smb.Dialect311}, SecurityMode: smb.AdvertisedSecurityMode,
				Contexts: []wire.NegotiateContext{preauth},
			})
			if err != nil {
				t.Fatal(err)
			}
			if err = client.Send(requestCtx, []wire.Message{{Header: wire.Header{Command: wire.Negotiate, Credit: 1}, Body: body}}); err != nil {
				t.Fatal(err)
			}
			reply, err := client.Receive(requestCtx)
			if err != nil {
				t.Fatal(err)
			}
			want := smb.StatusSuccess
			if encryption {
				want = smb.StatusNotSupported
			}
			if len(reply.Messages) != 1 || reply.Messages[0].Header.Status != want {
				t.Fatalf("negotiation: %+v, want %v", reply.Messages, want)
			}
			cancel()
			select {
			case err := <-r.serveDone:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("canceled Serve: %v", err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("server did not stop after cancellation")
			}
			if conn, err := net.DialTimeout("tcp", r.listener.Addr().String(), time.Second); err == nil {
				if err := conn.Close(); err != nil {
					t.Error(err)
				}
				t.Fatal("listener remained open")
			}
		})
	}
}

func TestSMBNextConstructorValidatesConfiguredIdentity(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*config.SMBConfig)
	}{
		{"user", func(c *config.SMBConfig) { c.Username = "" }},
		{"password", func(c *config.SMBConfig) { c.Password = "invalid\x00" }},
		{"share colon", func(c *config.SMBConfig) { c.Share = "invalid:share" }},
		{"share", func(c *config.SMBConfig) { c.Share = "IPC$" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			r, path := serverResources(t)
			c := serverConfig()
			test.change(&c)
			if err := r.startSMB(t.Context(), c, path); err == nil {
				t.Fatal("accepted invalid configured identity")
			}
			if r.server != nil || r.adapter != nil || r.listener != nil {
				t.Fatal("constructor returned resources on failure")
			}
		})
	}
}

func TestSMBNextAdapterConfig(t *testing.T) {
	for _, readOnly := range []bool{false, true} {
		r, path := serverResources(t)
		c := serverConfig()
		c.ReadOnly = readOnly
		if err := r.startSMB(t.Context(), c, path); err != nil {
			t.Fatal(err)
		}
		adapter, ok := r.adapter.(*smbfs.FS)
		if !ok {
			t.Fatalf("smbnext adapter type %T", r.adapter)
		}
		space, err := adapter.StatFS(t.Context())
		if err != nil || space.Capacity != r.runtime.Config.Format.Capacity {
			t.Fatalf("adapter capacity: %+v, %v", space, err)
		}
		resolved, err := adapter.Lookup(t.Context(), "fixture")
		if err != nil {
			t.Fatal(err)
		}
		_, err = adapter.Create(t.Context(), resolved.Name, smb.KindFile)
		if readOnly && !errors.Is(err, smb.ErrReadOnly) || !readOnly && err != nil {
			t.Fatalf("read_only=%t: create error %v", readOnly, err)
		}
	}
}

type closeOrderMetadata struct {
	meta.Meta
	closedOpen *bool
}

func (m closeOrderMetadata) CloseSession() error {
	if !*m.closedOpen {
		return errors.New("JuiceFS closed before SMB open")
	}
	return m.Meta.CloseSession()
}

type closeOrderStorage struct {
	smb.Storage
	closedOpen *bool
}

func (s closeOrderStorage) Close(ctx context.Context, handle smb.Handle) error {
	err := s.Storage.Close(ctx, handle)
	*s.closedOpen = true
	return err
}

func TestSMBNextShutdownClosesOpenBeforeJuiceFS(t *testing.T) {
	closedOpen := false
	r, path := serverResourcesWithMetadata(t, func(m meta.Meta) meta.Meta {
		return closeOrderMetadata{m, &closedOpen}
	})
	c := serverConfig()
	s, adapterCloser, err := newSMBServer(r.runtime, c, path)
	if err != nil {
		t.Fatal(err)
	}
	r.server, r.adapter = s, adapterCloser
	adapter, ok := r.adapter.(*smbfs.FS)
	if !ok {
		t.Fatalf("smbnext adapter type %T", r.adapter)
	}
	resolved, err := adapter.Lookup(t.Context(), "fixture")
	if err != nil {
		t.Fatal(err)
	}
	created, err := adapter.Create(t.Context(), resolved.Name, smb.KindFile)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := adapter.Open(t.Context(), created.Object, smb.AccessRead|smb.AccessWrite)
	if err != nil {
		t.Fatal(err)
	}
	table, err := state.New(time.Now)
	if err != nil {
		t.Fatal(err)
	}
	reservation, status := table.Reserve(state.OpenRequest{Object: created.Object, Binding: state.Binding{SessionID: 1, TreeID: 1}, GrantedAccess: 3, Sharing: 7})
	if status != smb.StatusSuccess {
		t.Fatal(status)
	}
	if _, status = table.Commit(reservation, state.Grant{Handle: handle}); status != smb.StatusSuccess {
		t.Fatal(status)
	}
	data := []byte("shutdown must flush this open")
	if n, err := adapter.WriteAt(t.Context(), handle, data, 0); err != nil || n != len(data) {
		t.Fatal(n, err)
	}
	// Replace the first server only to supply a table with an open before M3.
	r.server, err = server.New(server.Options{
		Storage: closeOrderStorage{adapter, &closedOpen}, State: table, Logger: slog.Default(), Now: time.Now,
		Account: auth.Account{User: c.Username, Password: c.Password}, ShareName: c.Share,
		ServerName: "s3-smb", ServerGUID: [16]byte{1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = r.close(); err != nil {
		t.Fatal(err)
	}
	*r = resources{}
	if !closedOpen {
		t.Fatal("server left the storage reference open")
	}
	if _, err = adapter.ReadAt(t.Context(), handle, make([]byte, 1), 0); !errors.Is(err, smb.ErrInvalidHandle) {
		t.Fatalf("open survived shutdown: %v", err)
	}
}
