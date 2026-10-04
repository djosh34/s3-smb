package server

import (
	"context"
	"io"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/juicefs/pkg/chunk"
	jfs "github.com/djosh34/s3-smb/internal/juicefs/pkg/fs"
	"github.com/djosh34/s3-smb/internal/juicefs/pkg/meta"
	"github.com/djosh34/s3-smb/internal/juicefs/pkg/object"
	"github.com/djosh34/s3-smb/internal/juicefs/pkg/vfs"
	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/smbtest"
	"github.com/djosh34/s3-smb/internal/smb/state"
	"github.com/djosh34/s3-smb/internal/smb/wire"
	"github.com/djosh34/s3-smb/internal/smbfs"
)

type ioStore struct {
	object.ObjectStorage
	put  func(context.Context, string, io.Reader) error
	puts atomic.Int64
	fail atomic.Bool
}

func (store *ioStore) Put(ctx context.Context, key string, src io.Reader, getters ...object.AttrGetter) error {
	store.puts.Add(1)
	if store.fail.Load() {
		return smb.ErrIO
	}
	if store.put != nil {
		return store.put(ctx, key, src)
	}
	return store.ObjectStorage.Put(ctx, key, src, getters...)
}

type ioFixture struct {
	adapter *smbfs.FS
	native  *jfs.FileSystem
	store   *ioStore
	config  *vfs.Config
}

func newIOFixture(t *testing.T, barrier smbfs.MetadataBarrier) *ioFixture {
	t.Helper()
	return newConfiguredIOFixture(t, barrier, nil, 0)
}

func newConfiguredIOFixture(t *testing.T, barrier smbfs.MetadataBarrier, configure func(string, *meta.Config, *chunk.Config, *ioStore), readWindow time.Duration) *ioFixture {
	t.Helper()
	dir := t.TempDir()
	blob, err := object.CreateStorage("file", filepath.Join(dir, "objects")+"/", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	store := &ioStore{ObjectStorage: blob}
	mc := meta.DefaultConf()
	mc.NoBGJob, mc.MaxDeletes, mc.Retries = true, 0, 0
	cc := chunk.Config{BlockSize: 64 << 10, MaxUpload: 2, MaxDownload: 2, BufferSize: 1 << 20, CacheSize: 0, MaxRetries: 1, GetTimeout: time.Second, PutTimeout: time.Second}
	if configure != nil {
		configure(dir, mc, &cc, store)
	}
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
	format := meta.Format{Name: "server-io", UUID: "server-io", Storage: "file", BlockSize: 64, Compression: "none", DirStats: true}
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
	chunks := chunk.NewCachedStore(store, cc, nil)
	config := &vfs.Config{Meta: mc, Format: format, Chunk: &cc}
	native, err := jfs.NewFileSystem(config, metadata, chunks, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := native.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if barrier == nil {
		barrier, err = smbfs.NewMetadataBarrier(database)
		if err != nil {
			t.Fatal(err)
		}
	}
	adapter, err := smbfs.New(smbfs.Options{Filesystem: native, Barrier: barrier, MetadataPath: database, Config: config, Store: chunks, ReadRetryWindow: readWindow})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := adapter.Shutdown(); err != nil {
			t.Error(err)
		}
	})
	return &ioFixture{adapter: adapter, native: native, store: store, config: config}
}

func insertIOOpen(t *testing.T, server *Server, session smbtest.Session, path string, granted uint32) state.Open {
	t.Helper()
	storage := server.options.Storage
	selected, err := storage.Lookup(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	if !selected.Exists {
		selected, err = storage.Create(t.Context(), selected.Name, smb.KindFile)
		if err != nil {
			t.Fatal(err)
		}
	}
	token, status := server.options.State.Reserve(state.OpenRequest{Object: selected.Object, Binding: state.Binding{SessionID: session.SessionID, TreeID: session.TreeID}, User: server.options.Account.User, Share: server.options.ShareName, GrantedAccess: granted, Sharing: 7})
	if status != smb.StatusSuccess {
		t.Fatal(status)
	}
	handle, err := storage.Open(t.Context(), selected.Object, smb.AccessRead|smb.AccessWrite)
	if err != nil {
		if abortStatus := server.options.State.Abort(token); abortStatus != smb.StatusSuccess {
			t.Error(abortStatus)
		}
		t.Fatal(err)
	}
	open, status := server.options.State.Commit(token, state.Grant{Handle: handle})
	if status != smb.StatusSuccess {
		if err := storage.Close(t.Context(), handle); err != nil {
			t.Error(err)
		}
		if abortStatus := server.options.State.Abort(token); abortStatus != smb.StatusSuccess {
			t.Error(abortStatus)
		}
		t.Fatal(status)
	}
	return open
}

func ioMessage(session smbtest.Session, id uint64, command wire.Command, body []byte, charge uint16) wire.Message {
	return wire.Message{Header: wire.Header{Command: command, MessageID: id, SessionID: session.SessionID, TreeID: session.TreeID, CreditCharge: charge, Credit: 32}, Body: body}
}

func ioRoundTrip(ctx context.Context, t *testing.T, client *smbtest.Client, message wire.Message) wire.Message {
	t.Helper()
	if err := client.Send(ctx, []wire.Message{message}); err != nil {
		t.Fatal(err)
	}
	response, err := client.Receive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Messages) != 1 {
		t.Fatal("expected one reply")
	}
	result := response.Messages[0]
	if result.Header.Status == smb.StatusPending {
		pending := result.Header
		response, err = client.Receive(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(response.Messages) != 1 {
			t.Fatal("expected one final reply")
		}
		result = response.Messages[0]
		if result.Header.AsyncID != pending.AsyncID || result.Header.Flags&wire.FlagAsync == 0 || result.Header.Credit != 0 {
			t.Fatal("invalid async completion")
		}
	}
	if result.Header.MessageID != message.Header.MessageID || result.Header.Command != message.Header.Command {
		t.Fatalf("wrong reply: %+v", result.Header)
	}
	return result
}

func TestIOFixtureRealHandle(t *testing.T) {
	fixture := newIOFixture(t, nil)
	options := testOptions(t)
	options.Storage = fixture.adapter
	server, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	client, ctx, session := loginClient(t, server, smb.CipherAES128GCM, smb.SigningGMAC)
	echoBody, err := wire.EncodeEchoRequest(wire.EmptyRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if response := ioRoundTrip(ctx, t, client, ioMessage(session, session.NextMessageID, wire.Echo, echoBody, 1)); response.Header.Status != smb.StatusSuccess {
		t.Fatal(response.Header)
	}
	open := insertIOOpen(t, server, session, "fixture", 3)
	if n, err := fixture.adapter.WriteAt(t.Context(), open.Handle, []byte("data"), 0); err != nil || n != 4 {
		t.Fatalf("write: %d, %v", n, err)
	}
	dst := make([]byte, 4)
	if n, err := fixture.adapter.ReadAt(t.Context(), open.Handle, dst, 0); err != nil || n != 4 || string(dst) != "data" {
		t.Fatalf("read: %d, %q, %v", n, dst, err)
	}
}
