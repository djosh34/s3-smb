package server

import (
	"time"

	"golang.org/x/sys/unix"
)

// setUserTimeout sets TCP_USER_TIMEOUT: how long sent data may stay
// unacknowledged before the kernel drops the connection.
func setUserTimeout(fd uintptr, timeout time.Duration) error {
	return unix.SetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_USER_TIMEOUT, int(timeout.Milliseconds()))
}
