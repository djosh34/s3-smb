package server

import (
	"fmt"
	"net"
	"time"
)

// A Mac that crashed, slept or lost its link comes back with a new client
// GUID, which the one-client rule lets in only once its old connection is
// gone. Keepalive probes after 15 idle seconds, every 15 seconds, 3 times,
// and a 60-second limit on unacknowledged data notice a dead link in about a
// minute, idle or with replies outstanding. They are set on the socket, not
// through the host's settings.
const (
	keepAliveIdle     = 15 * time.Second
	keepAliveInterval = 15 * time.Second
	keepAliveCount    = 3
	userTimeout       = time.Minute
)

// watchDeadLink sets keepalive and the user timeout on a TCP connection.
// Other connections, such as in-memory pipes in tests, are left alone.
func watchDeadLink(conn net.Conn) error {
	tcp, ok := conn.(*net.TCPConn)
	if !ok {
		return nil
	}
	if err := tcp.SetKeepAliveConfig(net.KeepAliveConfig{Enable: true, Idle: keepAliveIdle, Interval: keepAliveInterval, Count: keepAliveCount}); err != nil {
		return fmt.Errorf("set TCP keepalive: %w", err)
	}
	raw, err := tcp.SyscallConn()
	if err != nil {
		return fmt.Errorf("set TCP user timeout: %w", err)
	}
	var optErr error
	if err = raw.Control(func(fd uintptr) { optErr = setUserTimeout(fd, userTimeout) }); err != nil {
		return fmt.Errorf("set TCP user timeout: %w", err)
	}
	if optErr != nil {
		return fmt.Errorf("set TCP user timeout: %w", optErr)
	}
	return nil
}
