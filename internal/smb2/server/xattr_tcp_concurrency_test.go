package smb2

import (
	"bytes"
	"context"
	"net"
	"os"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/smb2/vfs"
	client "github.com/hirochachacha/go-smb2"
)

func TestResourceForkConcurrentSessionsOverTCP(t *testing.T) {
	f := &pausedXattrFS{rangeXattrFS: newRangeXattrFS([]byte("abcdef")), entered: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(func() {
		select {
		case <-f.release:
		default:
			close(f.release)
		}
	})
	l, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	s := NewServer(&ServerConfig{Xatrrs: true}, &NTLMAuthenticator{UserPassword: map[string]string{"backup": ""}}, map[string]vfs.VFSFileSystem{"backup": f})
	go func() { _ = s.ServeListener(l) }()
	defer func() {
		select {
		case <-f.release:
		default:
			close(f.release)
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if e := s.ShutdownContext(ctx); e != nil {
			t.Error(e)
		}
	}()
	var files []*client.File
	for i := 0; i < 2; i++ {
		session, c, e := dialWire(l, "backup", "", nil)
		if e != nil {
			t.Fatal(e)
		}
		defer c.Close()
		share, e := session.Mount("backup")
		if e != nil {
			t.Fatal(e)
		}
		file, e := share.OpenFile("fixture:AFP_Resource", os.O_RDWR, 0600)
		if e != nil {
			t.Fatal(e)
		}
		files = append(files, file)
	}
	firstDone, secondDone := make(chan error, 1), make(chan error, 1)
	go func() { _, e := files[0].WriteAt([]byte{'X'}, 1); firstDone <- e }()
	select {
	case <-f.entered:
	case <-time.After(time.Second):
		t.Fatal("first TCP write not paused")
	}
	go func() { _, e := files[1].WriteAt([]byte{'Y'}, 4); secondDone <- e }()
	early := false
	var secondErr error
	select {
	case secondErr = <-secondDone:
		early = true
	case <-time.After(20 * time.Millisecond):
	}
	close(f.release)
	select {
	case e := <-firstDone:
		if e != nil {
			t.Fatal(e)
		}
	case <-time.After(time.Second):
		t.Fatal("first TCP writer stuck")
	}
	if early {
		t.Fatalf("second session bypassed xattr lock: %v", secondErr)
	}
	select {
	case e := <-secondDone:
		if e != nil {
			t.Fatal(e)
		}
	case <-time.After(time.Second):
		t.Fatal("second TCP writer stuck")
	}
	data := make([]byte, 6)
	if n, e := files[0].ReadAt(data, 0); e != nil || n != 6 {
		t.Fatalf("read %d: %v", n, e)
	}
	if !bytes.Equal(data, []byte("aXcdYf")) {
		t.Fatalf("lost concurrent update: %q", data)
	}
	for _, file := range files {
		if e := file.Close(); e != nil {
			t.Fatal(e)
		}
	}
}
