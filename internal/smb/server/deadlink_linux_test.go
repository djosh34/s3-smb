package server

import (
	"net"
	"testing"

	"golang.org/x/sys/unix"
)

// Every SMB socket notices a dead link by itself: keepalive after 15 idle
// seconds, every 15 seconds, 3 times, and 60 seconds for unacknowledged data.
func TestDeadLinkSocketOptions(t *testing.T) {
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeOrFail(t, listener.Close) })
	dialed, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeOrFail(t, dialed.Close) })
	conn, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeOrFail(t, conn.Close) })
	if err = watchDeadLink(conn); err != nil {
		t.Fatal(err)
	}
	tcp, ok := conn.(*net.TCPConn)
	if !ok {
		t.Fatalf("accepted %T, not TCP", conn)
	}
	raw, err := tcp.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	for _, option := range []struct {
		name         string
		level, which int
		want         int
	}{
		{"SO_KEEPALIVE", unix.SOL_SOCKET, unix.SO_KEEPALIVE, 1},
		{"TCP_KEEPIDLE", unix.IPPROTO_TCP, unix.TCP_KEEPIDLE, 15},
		{"TCP_KEEPINTVL", unix.IPPROTO_TCP, unix.TCP_KEEPINTVL, 15},
		{"TCP_KEEPCNT", unix.IPPROTO_TCP, unix.TCP_KEEPCNT, 3},
		{"TCP_USER_TIMEOUT", unix.IPPROTO_TCP, unix.TCP_USER_TIMEOUT, 60000},
	} {
		var got int
		var getErr error
		if err = raw.Control(func(fd uintptr) { got, getErr = unix.GetsockoptInt(int(fd), option.level, option.which) }); err != nil || getErr != nil {
			t.Fatal(option.name, err, getErr)
		}
		if got != option.want {
			t.Errorf("%s = %d, want %d", option.name, got, option.want)
		}
	}
}

func closeOrFail(t *testing.T, closeFn func() error) {
	t.Helper()
	if err := closeFn(); err != nil {
		t.Error(err)
	}
}
