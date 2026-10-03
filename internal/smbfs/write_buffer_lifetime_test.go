// SPDX-License-Identifier: AGPL-3.0-only
package smbfs

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/juicefs/pkg/meta"
	"github.com/djosh34/s3-smb/internal/juicefs/pkg/object"
	server "github.com/djosh34/s3-smb/internal/smb2/server"
	"github.com/djosh34/s3-smb/internal/smb2/vfs"
	client "github.com/hirochachacha/go-smb2"
)

// Delays consumption, not just completion: Put must not touch r before release.
type delayedWriteStore struct {
	object.ObjectStorage
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	puts    atomic.Int32
}

func (s *delayedWriteStore) Put(ctx context.Context, key string, r io.Reader, getters ...object.AttrGetter) error {
	s.once.Do(func() { close(s.entered) })
	select {
	case <-s.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	s.puts.Add(1)
	return s.ObjectStorage.Put(ctx, key, r, getters...)
}

func lifetimeFixture(size, seed int) []byte {
	b := make([]byte, size)
	for i := range b {
		b[i] = byte((i*37 + i/251 + seed*53) % 251)
	}
	return b
}

func awaitLifetime(t *testing.T, ch <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", description)
	}
}

func TestWriteBufferLifetimeDelayedPut(t *testing.T) {
	for _, tc := range []struct {
		name string
		size int
		off  uint64
	}{
		{"one-byte", 1, 0},
		{"sub-page", 4095, 0},
		{"page", 4096, 0},
		{"block", 64 << 10, 0},
		{"block-tail", (64 << 10) + 1, 0},
		{"one-MiB", 1 << 20, 0},
		{"chunk-crossing", 997, meta.ChunkSize - 333},
	} {
		t.Run(tc.name, func(t *testing.T) {
			disk, err := object.CreateStorage("file", t.TempDir()+"/", "", "", "")
			if err != nil {
				t.Fatal(err)
			}
			store := &delayedWriteStore{ObjectStorage: disk, entered: make(chan struct{}), release: make(chan struct{})}
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(store.release) }) }
			defer release()
			f := fixtureWithStore(t, &failingStore{ObjectStorage: store}, nil)
			h := openFile(t, f.s, "lifetime")
			source := lifetimeFixture(tc.size, 1)
			want := bytes.Clone(source)
			if n, err := f.s.Write(h, source, tc.off, 0); err != nil || n != len(source) {
				t.Fatalf("write n=%d err=%v", n, err)
			}

			// The synchronous caller can now reuse its storage even while uploads
			// are pending. No mutation occurs while smbfs.Write borrows source.
			copy(source, lifetimeFixture(tc.size, 2))
			flushed := make(chan error, 1)
			go func() { flushed <- f.s.Flush(h) }()
			awaitLifetime(t, store.entered, "object Put to start without consuming its reader")
			if store.puts.Load() != 0 {
				t.Fatal("Put consumed before release")
			}
			copy(source, lifetimeFixture(tc.size, 3))
			release()
			select {
			case err := <-flushed:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("flush did not complete")
			}
			if store.puts.Load() == 0 {
				t.Fatal("no object Put consumed data")
			}

			// A new filesystem/cache checks persisted bytes, not a warm read cache.
			var dump bytes.Buffer
			if err := f.m.DumpMeta(&dump, meta.RootInode, 1, true, false, false); err != nil {
				t.Fatal(err)
			}
			cold := fixtureWithStore(t, f.store, dump.Bytes(), true)
			ch, err := cold.s.Open("lifetime", syscall.O_RDONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			got := make([]byte, len(want))
			if n, err := cold.s.Read(ch, got, tc.off, 0); err != nil || n != len(got) {
				t.Fatalf("cold read n=%d err=%v", n, err)
			}
			if !bytes.Equal(got, want) {
				t.Fatal("persisted bytes changed after caller buffer reuse")
			}
		})
	}
}

// Holds the first WRITE's borrowed request data while later network requests
// run. It deliberately does not copy b before the gate.
type delayedIngressFS struct {
	*FS
	first   atomic.Bool
	entered chan []byte
	release chan struct{}
}

func (f *delayedIngressFS) Write(h vfs.VfsHandle, b []byte, off uint64, flags int) (int, error) {
	if f.first.CompareAndSwap(false, true) {
		f.entered <- b
		<-f.release
	}
	return f.FS.Write(h, b, off, flags)
}

func TestWriteBufferLifetimeDelayedSignedIngress(t *testing.T) {
	f := newFixture(t)
	gate := &delayedIngressFS{FS: f.s, entered: make(chan []byte, 1), release: make(chan struct{})}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(gate.release) }) }
	defer release()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := server.NewServer(&server.ServerConfig{MaxIOWrites: 2}, &server.NTLMAuthenticator{UserPassword: map[string]string{"fixture": "fixture-only-password"}}, map[string]vfs.VFSFileSystem{"fixture": gate})
	go func() { _ = srv.ServeListener(listener) }()
	t.Cleanup(func() {
		release()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.ShutdownContext(ctx); err != nil {
			t.Errorf("server shutdown: %v", err)
		}
	})
	network, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = network.Close() })
	_ = network.SetDeadline(time.Now().Add(20 * time.Second))
	dialer := &client.Dialer{Negotiator: client.Negotiator{RequireMessageSigning: true}, Initiator: &client.NTLMInitiator{User: "fixture", Password: "fixture-only-password"}}
	session, err := dialer.Dial(network)
	if err != nil {
		t.Fatal(err)
	}
	share, err := session.Mount("fixture")
	if err != nil {
		t.Fatal(err)
	}
	a, err := share.OpenFile("held", syscall.O_CREAT|syscall.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	b, err := share.OpenFile("later", syscall.O_CREAT|syscall.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	want := lifetimeFixture(64<<10, 10)
	written := make(chan error, 1)
	go func() {
		n, err := a.WriteAt(want, 0)
		if err == nil && n != len(want) {
			err = fmt.Errorf("first write n=%d want=%d", n, len(want))
		}
		written <- err
	}()
	var borrowed []byte
	select {
	case borrowed = <-gate.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first WRITE did not reach delayed consumer")
	}
	if !bytes.Equal(borrowed, want) {
		t.Fatal("first request did not contain expected safe fixture")
	}

	// Same-size requests particularly exercise hypothetical receive-buffer
	// reuse. Source storage is reused only after each client WriteAt returns.
	source := make([]byte, len(want))
	for i := 0; i < 12; i++ {
		copy(source, lifetimeFixture(len(source), 20+i))
		if n, err := b.WriteAt(source, int64(i*len(source))); err != nil || n != len(source) {
			t.Fatalf("later write %d n=%d err=%v", i, n, err)
		}
	}
	runtime.GC()
	if !bytes.Equal(borrowed, want) {
		t.Fatal("delayed WRITE request bytes changed while later frames were received")
	}
	release()
	select {
	case err := <-written:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first WRITE did not finish")
	}
	if err := a.Sync(); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(want))
	if n, err := a.ReadAt(got, 0); err != nil || n != len(got) {
		t.Fatalf("first read n=%d err=%v", n, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("delayed first WRITE stored later request bytes")
	}
	for i := 0; i < 12; i++ {
		if n, err := b.ReadAt(got, int64(i*len(got))); err != nil || n != len(got) {
			t.Fatalf("later read %d n=%d err=%v", i, n, err)
		}
		if !bytes.Equal(got, lifetimeFixture(len(got), 20+i)) {
			t.Fatalf("later request %d fixture mismatch", i)
		}
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	if err := share.Umount(); err != nil {
		t.Fatal(err)
	}
	if err := session.Logoff(); err != nil {
		t.Fatal(err)
	}
}
