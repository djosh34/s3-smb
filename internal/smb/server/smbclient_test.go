package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"testing"
	"time"
)

// This needs no file handlers or main wiring. The CI test image supplies the
// Samba client; developer machines without it skip this interoperability test.
func TestSmbclientAuthenticatesAndConnectsWithoutListing(t *testing.T) {
	if _, err := exec.LookPath("smbclient"); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			t.Skip("smbclient is not installed")
		}
		t.Fatal(err)
	}
	for _, encrypted := range []bool{false, true} {
		t.Run(fmt.Sprintf("encrypted_%t", encrypted), func(t *testing.T) { checkSmbclientLogin(t, encrypted) })
	}
}

func checkSmbclientLogin(t *testing.T, encrypted bool) {
	t.Helper()
	options := testOptions(t)
	protection := "encrypt"
	if !encrypted {
		options.Encryption = AllowPlaintext
		protection = "sign"
	}
	server, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx, listener) }()
	t.Cleanup(func() {
		cancel()
		if shutdownErr := server.Shutdown(context.WithoutCancel(ctx)); shutdownErr != nil {
			t.Error(shutdownErr)
		}
		if serveErr := <-done; serveErr != nil && !errors.Is(serveErr, context.Canceled) {
			t.Error(serveErr)
		}
	})
	host, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(ctx, "smbclient", "//"+host+"/"+options.ShareName, "-p", port, "-U", options.Account.User, //nolint:gosec // Arguments use fixed test credentials and the test's loopback listener.
		"--option=client min protocol=SMB3_11", "--option=client max protocol=SMB3_11", "--client-protection="+protection, "-c", "quit")
	command.Env = append(os.Environ(), "PASSWD="+options.Account.Password)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("smbclient login: %v\n%s", err, output)
	}
}
