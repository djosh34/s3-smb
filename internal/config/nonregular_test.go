package config

import (
	"context"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestFIFORejectedWithoutBlocking(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fifo")
	if err := syscall.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := (SecretSource{File: &path}).resolve(context.Background(), ".", "credential", quietLogger())
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("FIFO secret accepted")
		}
	case <-time.After(time.Second):
		t.Fatal("opening secret FIFO blocked")
	}
	go func() { _, err := Load(path); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("FIFO config accepted")
		}
	case <-time.After(time.Second):
		t.Fatal("opening config FIFO blocked")
	}
}
