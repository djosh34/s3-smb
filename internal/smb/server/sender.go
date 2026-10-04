package server

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"
)

type sendJob struct {
	result chan error
	frame  []byte
}

// sender has one writer and a separate completion for every accepted frame.
// Producers give it owned bytes and wait on their own result, never a shared one.
type sender struct {
	conn     net.Conn
	terminal error
	wake     chan struct{}
	done     chan struct{}
	queue    []sendJob
	mu       sync.Mutex
}

func newSender(conn net.Conn) *sender {
	return &sender{conn: conn, wake: make(chan struct{}, 1), done: make(chan struct{})}
}

func (sender *sender) enqueue(payload []byte) <-chan error {
	result := make(chan error, 1)
	if len(payload) == 0 || len(payload) > 0xffffff {
		result <- errors.New("invalid outgoing frame length")
		return result
	}
	frame := make([]byte, 4, len(payload)+4)
	binary.BigEndian.PutUint32(frame, uint32(len(payload)&0xffffff))
	frame = append(frame, payload...)
	sender.mu.Lock()
	defer sender.mu.Unlock()
	if sender.terminal != nil {
		result <- sender.terminal
		return result
	}
	sender.queue = append(sender.queue, sendJob{frame: frame, result: result})
	select {
	case sender.wake <- struct{}{}:
	default:
	}
	return result
}

func (sender *sender) take() (sendJob, bool) {
	sender.mu.Lock()
	defer sender.mu.Unlock()
	if len(sender.queue) == 0 {
		return sendJob{}, false
	}
	job := sender.queue[0]
	sender.queue[0] = sendJob{}
	sender.queue = sender.queue[1:]
	return job, true
}

func (sender *sender) stop(err error) {
	sender.mu.Lock()
	defer sender.mu.Unlock()
	sender.terminal = err
	for _, job := range sender.queue {
		job.result <- err
	}
	sender.queue = nil
}

func (sender *sender) run(ctx context.Context, closeConn func() error) {
	defer close(sender.done)
	for {
		if err := ctx.Err(); err != nil {
			sender.stop(errors.Join(err, closeConn()))
			return
		}
		job, ok := sender.take()
		if !ok {
			select {
			case <-sender.wake:
			case <-ctx.Done():
			}
			continue
		}
		err := writeAll(sender.conn, job.frame)
		if err != nil {
			err = errors.Join(err, closeConn())
			// Publish the terminal state before reporting the current completion.
			// No later producer may put another frame on the broken stream.
			sender.stop(err)
			job.result <- err
			return
		}
		job.result <- nil
	}
}

func writeAll(writer io.Writer, frame []byte) error {
	for len(frame) > 0 {
		n, err := writer.Write(frame)
		if err != nil {
			return err
		}
		if n <= 0 || n > len(frame) {
			return io.ErrShortWrite
		}
		frame = frame[n:]
	}
	return nil
}
