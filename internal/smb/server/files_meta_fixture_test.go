package server

import (
	"context"
	"net"
	"path/filepath"
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
	"github.com/djosh34/s3-smb/internal/smbfs"
)

// newFilesMetaStorage uses file-backed JuiceFS data and SQLite metadata.
// Register server cleanup after this helper so opens close before storage.
func newFilesMetaStorage(t *testing.T) *smbfs.FS {
	t.Helper()
	return newFilesMetaStorageWithCapacity(t, 0)
}

func newFilesMetaStorageWithCapacity(t *testing.T, capacity uint64) *smbfs.FS {
	t.Helper()
	dir := t.TempDir()
	blob, err := object.CreateStorage("file", filepath.Join(dir, "objects")+"/", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	conf := meta.DefaultConf()
	conf.NoBGJob = true
	conf.MaxDeletes = 0
	conf.Retries = 0
	database := filepath.Join(dir, "meta.db")
	metadata, err := meta.NewSQLite(database, conf)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if shutdownErr := metadata.Shutdown(); shutdownErr != nil {
			t.Error(shutdownErr)
		}
	})
	format := meta.Format{Name: "files-meta-test", UUID: "files-meta-fixture", Storage: "file", BlockSize: 64, Compression: "none", DirStats: true, Capacity: capacity}
	if err = metadata.Init(&format, true); err != nil {
		t.Fatal(err)
	}
	root := meta.Attr{Uid: smbfs.UID, Gid: smbfs.GID, Mode: 0o700}
	if errno := metadata.SetAttr(meta.Background(), meta.RootInode, meta.SetAttrUID|meta.SetAttrGID|meta.SetAttrMode, 0, &root); errno != 0 {
		t.Fatal(errno)
	}
	if err = metadata.NewSession(true); err != nil {
		t.Fatal(err)
	}
	chunks := chunk.Config{BlockSize: 64 << 10, MaxUpload: 2, MaxDownload: 2, BufferSize: 1 << 20, CacheSize: 0, MaxRetries: 1, GetTimeout: 5 * time.Second, PutTimeout: time.Second}
	store := chunk.NewCachedStore(blob, chunks, nil)
	config := &vfs.Config{Meta: conf, Format: format, Chunk: &chunks}
	filesystem, err := jfs.NewFileSystem(config, metadata, store, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := filesystem.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	barrier, err := smbfs.NewMetadataBarrier(database)
	if err != nil {
		t.Fatal(err)
	}
	storage, err := smbfs.New(smbfs.Options{Filesystem: filesystem, Barrier: barrier, MetadataPath: database, Config: config, Store: store, Capacity: capacity})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := storage.Shutdown(); err != nil {
			t.Error(err)
		}
	})
	return storage
}

// newFilesMetaClient drives the public connection entry point with raw SMB.
func newFilesMetaClient(t *testing.T, server *Server) (*smbtest.Client, context.Context, smbtest.Session) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	local, remote := net.Pipe()
	client, err := smbtest.NewClient(remote)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if serveErr := server.ServeConn(ctx, local); serveErr != nil {
			server.options.Logger.Debug("test connection ended", "error", serveErr)
		}
	}()
	t.Cleanup(func() {
		if closeErr := client.Close(); closeErr != nil {
			t.Error(closeErr)
		}
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("ServeConn did not stop")
		}
		if shutdownErr := server.Shutdown(context.WithoutCancel(t.Context())); shutdownErr != nil {
			t.Error(shutdownErr)
		}
	})
	session, err := client.Login(ctx, smbtest.LoginOptions{Share: server.options.ShareName, Account: server.options.Account, Cipher: smb.CipherAES256GCM, Signing: smb.SigningGMAC, ClientGUID: [16]byte{2}})
	if err != nil {
		t.Fatal(err)
	}
	return client, ctx, session
}

func insertFilesMetaOpen(t *testing.T, server *Server, session smbtest.Session, path string, grantedAccess uint32) state.Open {
	t.Helper()
	storage := server.options.Storage
	resolved, err := storage.Lookup(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	if !resolved.Exists {
		resolved, err = storage.Create(t.Context(), resolved.Name, smb.KindFile)
		if err != nil {
			t.Fatal(err)
		}
	}
	handle, err := storage.Open(t.Context(), resolved.Object, smb.AccessRead|smb.AccessWrite)
	if err != nil {
		t.Fatal(err)
	}
	reservation, status := server.options.State.Reserve(state.OpenRequest{
		Object: resolved.Object, Binding: state.Binding{SessionID: session.SessionID, TreeID: session.TreeID},
		User: server.options.Account.User, Share: server.options.ShareName, ClientGUID: state.GUID{2},
		GrantedAccess: grantedAccess, Sharing: 7,
	})
	if status != smb.StatusSuccess {
		if closeErr := storage.Close(context.WithoutCancel(t.Context()), handle); closeErr != nil {
			t.Error(closeErr)
		}
		t.Fatal(status)
	}
	open, status := server.options.State.Commit(reservation, state.Grant{Handle: handle, Directory: resolved.Attr.Kind == smb.KindDirectory})
	if status != smb.StatusSuccess {
		if abortStatus := server.options.State.Abort(reservation); abortStatus != smb.StatusSuccess {
			t.Error(abortStatus)
		}
		if closeErr := storage.Close(context.WithoutCancel(t.Context()), handle); closeErr != nil {
			t.Error(closeErr)
		}
		t.Fatal(status)
	}
	return open
}

func TestFilesMetaStorageRoot(t *testing.T) {
	storage := newFilesMetaStorage(t)
	root, err := storage.Lookup(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	if !root.Exists || root.Attr.Kind != smb.KindDirectory || root.Object.Inode == 0 {
		t.Fatalf("root = %+v", root)
	}
}
