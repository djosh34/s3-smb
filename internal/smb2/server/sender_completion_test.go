package smb2

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	. "github.com/djosh34/s3-smb/internal/smb2/internal/smb2"
)

// Observe the actual transport result without changing it or delaying writes.
// The receiver uses a real TCP socket; no fabricated short writes are involved.
type observedSenderTransport struct {
	transport
	mu        sync.Mutex
	completed map[uint64]bool
}

func (s *observedSenderTransport) Write(p []byte) (int, error) {
	n, err := s.transport.Write(p)
	s.mu.Lock()
	s.completed[binary.LittleEndian.Uint64(p[24:32])] = true
	s.mu.Unlock()
	return n, err
}

func TestConcurrentSenderWaitsForOwnWrite(t *testing.T) {
	// Concurrent queue producers can otherwise consume one another's werr.
	// Pin two processors to exercise the enqueue/await preemption window even
	// when the test command limits other build/test concurrency to one process.
	oldProcs := runtime.GOMAXPROCS(2)
	defer runtime.GOMAXPROCS(oldProcs)
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	client, err := net.Dial("tcp4", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	peer, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	if err := peer.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	observed := &observedSenderTransport{transport: direct(peer), completed: make(map[uint64]bool)}
	c := &conn{
		t: observed, ctx: ctx, cancel: cancel, account: openAccount(16),
		serverState: STATE_SESSION_ACTIVE,
		write:       make(chan []byte, 10), werr: make(chan error, 1),
		wdone: make(chan struct{}, 1), rdone: make(chan struct{}, 1),
	}
	done := make(chan struct{})
	go func() { defer close(done); c.runSender() }()
	defer func() {
		c.shutdown()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("sender failed to drain")
		}
	}()
	readDone := make(chan struct{})
	go func() { defer close(readDone); _, _ = io.Copy(io.Discard, client) }()

	const producers, perProducer = 16, 512
	var producersWG sync.WaitGroup
	var early atomic.Int64
	for producer := 0; producer < producers; producer++ {
		producersWG.Add(1)
		go func(producer int) {
			defer producersWG.Done()
			for i := 0; i < perProducer; i++ {
				id := uint64(producer*perProducer + i + 1)
				rsp := &EchoResponse{PacketHeader: PacketHeader{MessageId: id, CreditCharge: 1}}
				if err := c.sendPacket(rsp, nil, nil); err != nil {
					t.Errorf("send: %v", err)
					return
				}
				observed.mu.Lock()
				completed := observed.completed[id]
				observed.mu.Unlock()
				if !completed {
					early.Add(1)
				}
			}
		}(producer)
	}
	producersWG.Wait()
	c.shutdown()
	<-readDone
	if count := early.Load(); count != 0 {
		t.Fatalf("%d sendPacket callers returned success before their own transport.Write completed", count)
	}
}
