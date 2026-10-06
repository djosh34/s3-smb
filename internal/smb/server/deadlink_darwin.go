package server

import (
	"time"

	"golang.org/x/sys/unix"
)

// setUserTimeout sets TCP_RXT_CONNDROPTIME, macOS's counterpart of Linux's
// TCP_USER_TIMEOUT: how long retransmissions may go unacknowledged before
// the kernel drops the connection.
func setUserTimeout(fd uintptr, timeout time.Duration) error {
	return unix.SetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_RXT_CONNDROPTIME, int(timeout.Seconds()))
}
