package smbtest_test

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/smb/smbtest"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

// shortWriter forces the client to loop and makes interleaved sends observable.
type shortWriter struct{ net.Conn }

func (conn shortWriter) Write(data []byte) (int, error) {
	if len(data) > 2 {
		data = data[:2]
	}
	return conn.Conn.Write(data)
}

func TestConcurrentSendsAndShortWrites(t *testing.T) {
	client := fakePeerConn(t, func(conn net.Conn) net.Conn { return shortWriter{conn} }, func(peer net.Conn) error {
		seen := make(map[uint64]bool)
		for range 8 {
			payload, err := readFrame(peer)
			if err != nil {
				return err
			}
			messages, err := wire.Split(payload)
			if err != nil {
				return err
			}
			id := messages[0].Header.MessageID
			if len(messages) != 1 || id >= 8 || seen[id] {
				return errors.New("interleaved or duplicate frame")
			}
			seen[id] = true
		}
		return nil
	})
	results := make(chan error, 8)
	for id := range uint64(8) {
		go func() {
			results <- client.Send(t.Context(), []wire.Message{{Header: wire.Header{Command: wire.Echo, MessageID: id}, Body: []byte{4, 0, 0, 0}}})
		}()
	}
	for range 8 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
}

type observedConn struct {
	net.Conn
	started chan struct{}
	once    sync.Once
}

func (conn *observedConn) Read(data []byte) (int, error) {
	conn.once.Do(func() { close(conn.started) })
	return conn.Conn.Read(data)
}

func (conn *observedConn) Write(data []byte) (int, error) {
	conn.once.Do(func() { close(conn.started) })
	return conn.Conn.Write(data)
}

func TestCancellationInterruptsIO(t *testing.T) {
	for _, direction := range []string{"read", "write"} {
		t.Run(direction, func(t *testing.T) {
			conn, peer := net.Pipe()
			observed := &observedConn{Conn: conn, started: make(chan struct{})}
			client, err := smbtest.NewClient(observed)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := errors.Join(client.Close(), peer.Close()); err != nil {
					t.Error(err)
				}
			})
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			result := make(chan error, 1)
			go func() {
				if direction == "write" {
					result <- client.SendRaw(ctx, []byte{0, 0, 0, 1, 42})
				} else {
					_, err := client.ReceiveRaw(ctx)
					result <- err
				}
			}()
			<-observed.started
			cancel()
			select {
			case err := <-result:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("canceled I/O = %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("cancellation did not unblock I/O")
			}
			if err := client.SendRaw(t.Context(), []byte{1}); err == nil {
				t.Fatal("connection remained usable after interrupted I/O")
			}
		})
	}
}

func TestContextDeadline(t *testing.T) {
	client := fakePeer(t, func(peer net.Conn) error {
		var data [1]byte
		_, err := peer.Read(data[:])
		if !errors.Is(err, io.EOF) {
			return err
		}
		return nil
	})
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if _, err := client.ReceiveRaw(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline error = %v", err)
	}
}

func TestValidationDoesNotWriteOrClose(t *testing.T) {
	client := fakePeer(t, func(peer net.Conn) error {
		payload, err := readFrame(peer)
		if err != nil {
			return err
		}
		_, err = wire.Split(payload)
		return err
	})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := client.SendRaw(ctx, []byte{0xff}); !errors.Is(err, context.Canceled) {
		t.Fatalf("already canceled send = %v", err)
	}
	if _, err := client.ReceiveRaw(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("already canceled receive = %v", err)
	}
	for _, messages := range [][]wire.Message{
		nil,
		{{Header: wire.Header{AsyncID: 1}}},
		{{Body: make([]byte, 0xffffff-63)}},
	} {
		if err := client.Send(t.Context(), messages); err == nil {
			t.Fatal("invalid send accepted")
		}
	}
	if err := client.Send(t.Context(), []wire.Message{{Header: wire.Header{Command: wire.Echo}}}); err != nil {
		t.Fatalf("valid send after validation error: %v", err)
	}
}

func TestBrokenFrames(t *testing.T) {
	for name, framed := range map[string][]byte{
		"frame type":    {1, 0, 0, 0},
		"short prefix":  {0, 0},
		"short payload": {0, 0, 0, 4, 42},
	} {
		t.Run(name, func(t *testing.T) {
			client := fakePeer(t, func(peer net.Conn) error {
				if _, err := peer.Write(framed); err != nil {
					return err
				}
				return peer.Close()
			})
			if _, err := client.ReceiveRaw(t.Context()); err == nil {
				t.Fatal("broken frame accepted")
			}
		})
	}
}

var errInjected = errors.New("injected transport failure")

type failedConn struct {
	net.Conn
	fail       string
	writeCount int
	closeCount int
}

func (conn *failedConn) Write(data []byte) (int, error) {
	conn.writeCount++
	if conn.fail == "zero write" {
		return 0, nil
	}
	return len(data) / 2, errInjected
}

func (conn *failedConn) SetWriteDeadline(time.Time) error {
	if conn.fail == "deadline" {
		return errInjected
	}
	return nil
}

func (conn *failedConn) Close() error {
	conn.closeCount++
	return errInjected
}

func TestTransportAndCleanupErrors(t *testing.T) {
	for _, failure := range []string{"partial write", "zero write", "deadline"} {
		t.Run(failure, func(t *testing.T) {
			conn := &failedConn{fail: failure}
			client, err := smbtest.NewClient(conn)
			if err != nil {
				t.Fatal(err)
			}
			err = client.SendRaw(t.Context(), []byte{0, 0, 0, 1, 42})
			if !errors.Is(err, errInjected) {
				t.Fatalf("transport and close error = %v", err)
			}
			if failure == "zero write" && !errors.Is(err, io.ErrNoProgress) {
				t.Fatalf("zero write error = %v", err)
			}
			wantWrites := 1
			if failure == "deadline" {
				wantWrites = 0
			}
			if conn.writeCount != wantWrites {
				t.Fatalf("writes = %d, want %d", conn.writeCount, wantWrites)
			}
			if !errors.Is(client.Close(), errInjected) || conn.closeCount != 1 {
				t.Fatalf("Close did not retain its error or closed %d times", conn.closeCount)
			}
		})
	}
}
