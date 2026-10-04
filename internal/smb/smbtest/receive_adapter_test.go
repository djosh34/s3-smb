package smbtest_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/juicefs/pkg/chunk"
	jfs "github.com/djosh34/s3-smb/internal/juicefs/pkg/fs"
	"github.com/djosh34/s3-smb/internal/juicefs/pkg/meta"
	"github.com/djosh34/s3-smb/internal/juicefs/pkg/object"
	"github.com/djosh34/s3-smb/internal/juicefs/pkg/vfs"
	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/auth"
	"github.com/djosh34/s3-smb/internal/smb/server"
	"github.com/djosh34/s3-smb/internal/smb/smbtest"
	"github.com/djosh34/s3-smb/internal/smb/state"
	"github.com/djosh34/s3-smb/internal/smb/wire"
	"github.com/djosh34/s3-smb/internal/smbfs"
)

func TestReceivePolicySurvivesReconnectWithRealAdapter(t *testing.T) {
	adapter := receiveAdapter(t)
	for _, test := range []struct {
		name    string
		cipher  uint16
		signing uint16
		policy  server.EncryptionPolicy
	}{
		{"signed CMAC", 0, smb.SigningCMAC, server.AllowPlaintext},
		{"signed GMAC", 0, smb.SigningGMAC, server.AllowPlaintext},
		{"voluntary 128", smb.CipherAES128GCM, smb.SigningGMAC, server.AllowPlaintext},
		{"voluntary 256", smb.CipherAES256GCM, smb.SigningCMAC, server.AllowPlaintext},
		{"required 128", smb.CipherAES128GCM, smb.SigningGMAC, server.RequireEncryption},
		{"required 256", smb.CipherAES256GCM, smb.SigningCMAC, server.RequireEncryption},
	} {
		t.Run(test.name, func(t *testing.T) {
			checkReceivePolicyReconnect(t, adapter, test.cipher, test.signing, test.policy)
		})
	}
}

func checkReceivePolicyReconnect(t *testing.T, adapter *smbfs.FS, cipher, signing uint16, policy server.EncryptionPolicy) {
	t.Helper()
	table, err := state.New(time.Now)
	if err != nil {
		t.Fatal(err)
	}
	account := auth.Account{User: "backup", Password: "password"}
	srv, err := server.New(server.Options{Storage: adapter, State: table, Account: account, ShareName: "backup", ServerName: "server", ServerGUID: [16]byte{1}, Now: time.Now, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Encryption: policy})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
		defer cancel()
		if shutdownErr := srv.Shutdown(ctx); shutdownErr != nil {
			t.Error(shutdownErr)
		}
	})
	old, err := smbtest.NewClient(inProcessConn(t, srv))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := old.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	options := smbtest.LoginOptions{Share: "backup", Account: account, Cipher: cipher, Signing: signing}
	session, err := old.Login(t.Context(), options)
	if err != nil {
		t.Fatal(err)
	}
	checkAdapterLeaseBreak(t, adapter, srv, table, old, session)
	if closeErr := old.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	previous := session
	client, session, _, err := smbtest.Reconnect(t.Context(), inProcessConn(t, srv), previous, options, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := client.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if session.SessionID == previous.SessionID || session.ClientGUID != previous.ClientGUID || session.Cipher != previous.Cipher {
		t.Fatalf("reconnect identity = %+v, previous = %+v", session, previous)
	}
	checkAdapterLeaseBreak(t, adapter, srv, table, client, session)
}

func checkAdapterLeaseBreak(t *testing.T, adapter *smbfs.FS, srv *server.Server, table *state.Table, client *smbtest.Client, session smbtest.Session) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	resolved, err := adapter.Lookup(ctx, fmt.Sprintf("%s-%d", strings.ReplaceAll(t.Name(), "/", "-"), session.SessionID))
	if err != nil {
		t.Fatal(err)
	}
	resolved, err = adapter.Create(ctx, resolved.Name, smb.KindFile)
	if err != nil {
		t.Fatal(err)
	}
	guid := state.GUID(session.ClientGUID)
	reservation, status := table.Reserve(state.OpenRequest{Object: resolved.Object, Binding: state.Binding{SessionID: session.SessionID, TreeID: session.TreeID}, User: "backup", Share: "backup", ClientGUID: guid, GrantedAccess: 1, Sharing: 7})
	if status != smb.StatusSuccess {
		t.Fatal(status)
	}
	handle, err := adapter.Open(ctx, resolved.Object, smb.AccessRead)
	if err != nil {
		if abortStatus := table.Abort(reservation); abortStatus != smb.StatusSuccess {
			t.Errorf("abort: %#x", abortStatus)
		}
		t.Fatal(err)
	}
	key := state.GUID{2}
	_, status = table.Commit(reservation, state.Grant{Handle: handle, Lease: state.Lease{ClientGUID: guid, Key: key, State: smb.LeaseRead, Epoch: 7}})
	if status != smb.StatusSuccess {
		if abortStatus := table.Abort(reservation); abortStatus != smb.StatusSuccess {
			t.Errorf("abort: %#x", abortStatus)
		}
		t.Fatal(errors.Join(fmt.Errorf("lease commit: %#x", status), adapter.Close(ctx, handle)))
	}
	done := make(chan error, 1)
	go func() { done <- srv.BreakLeases(ctx, resolved.Object, state.GUID{9}, state.GUID{9}, 0) }()
	notification, err := client.WaitLeaseBreak(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := wire.LeaseBreakNotification{Key: [16]byte(key), CurrentState: smb.LeaseRead, Epoch: 8}
	if notification != want {
		t.Fatalf("R-only notification = %+v, want %+v", notification, want)
	}
	select {
	case breakErr := <-done:
		if breakErr != nil {
			t.Fatal(breakErr)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	message := wire.Message{Header: wire.Header{Command: wire.Echo, MessageID: session.NextMessageID, SessionID: session.SessionID, CreditCharge: 1, Credit: 1}, Body: []byte{4, 0, 0, 0}}
	if sendErr := client.Send(ctx, []wire.Message{message}); sendErr != nil {
		t.Fatal(sendErr)
	}
	reply, err := client.Receive(ctx)
	if err != nil || len(reply.Messages) != 1 || reply.Messages[0].Header.Status != smb.StatusSuccess {
		t.Fatalf("ECHO after notification = %+v, error = %v", reply, err)
	}
	if transformed := bytes.HasPrefix(reply.Raw, []byte{0xfd, 'S', 'M', 'B'}); transformed != (session.Cipher != 0) {
		t.Fatal("reply protection differs from request protection")
	}
}

func receiveAdapter(t *testing.T) *smbfs.FS {
	t.Helper()
	dir := t.TempDir()
	blob, err := object.CreateStorage("file", filepath.Join(dir, "objects")+"/", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	mc := meta.DefaultConf()
	mc.NoBGJob, mc.MaxDeletes, mc.Retries = true, 0, 0
	database := filepath.Join(dir, "meta.db")
	metadata, err := meta.NewSQLite(database, mc)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if shutdownErr := metadata.Shutdown(); shutdownErr != nil {
			t.Error(shutdownErr)
		}
	})
	format := meta.Format{Name: "receive-policy", UUID: "receive-policy", Storage: "file", BlockSize: 64, Compression: "none", DirStats: true}
	if initErr := metadata.Init(&format, true); initErr != nil {
		t.Fatal(initErr)
	}
	root := meta.Attr{Uid: smbfs.UID, Gid: smbfs.GID, Mode: 0o700}
	if eno := metadata.SetAttr(meta.Background(), meta.RootInode, meta.SetAttrUID|meta.SetAttrGID|meta.SetAttrMode, 0, &root); eno != 0 {
		t.Fatal(eno)
	}
	if sessionErr := metadata.NewSession(true); sessionErr != nil {
		t.Fatal(sessionErr)
	}
	cc := chunk.Config{BlockSize: 64 << 10, MaxUpload: 2, MaxDownload: 2, BufferSize: 1 << 20, CacheSize: 0, MaxRetries: 1, GetTimeout: time.Second, PutTimeout: time.Second}
	store := chunk.NewCachedStore(blob, cc, nil)
	config := &vfs.Config{Meta: mc, Format: format, Chunk: &cc}
	native, err := jfs.NewFileSystem(config, metadata, store, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := native.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	barrier, err := smbfs.NewMetadataBarrier(database)
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := smbfs.New(smbfs.Options{Filesystem: native, Barrier: barrier, MetadataPath: database, Config: config, Store: store})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if shutdownErr := adapter.Shutdown(); shutdownErr != nil {
			t.Error(shutdownErr)
		}
	})
	return adapter
}
