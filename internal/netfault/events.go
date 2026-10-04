// SPDX-License-Identifier: AGPL-3.0-only

package netfault

import (
	"io"
	"net"
)

// Direction identifies traffic without inspecting its contents. Zero identifies
// a manual cut or drop, which has no traffic direction.
type Direction uint8

const (
	// ClientToServer identifies bytes forwarded to the upstream peer.
	ClientToServer Direction = iota + 1
	// ServerToClient identifies bytes forwarded back to the client.
	ServerToClient
)

// Event reports forwarded bytes or a cut for one connection. Bytes is the number
// forwarded in this event, not a cumulative count. A byte-cut event includes the
// final forwarded bytes; manual cuts and drops have zero Direction and Bytes.
type Event struct {
	Connection uint64
	Bytes      int64
	Direction  Direction
	Cut        bool
}

// Events returns a bounded, nonblocking stream. Events may be lost if the reader
// falls behind; DroppedEvents counts every loss, including cut events. Close
// closes the stream after all forwarding workers finish. Events contain no data.
func (p *Proxy) Events() <-chan Event { return p.events }

// DroppedEvents returns the number of events lost to a full stream buffer.
func (p *Proxy) DroppedEvents() uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.dropped
}

// emit runs under mu, including during fault replacement and forwarding.
func (p *Proxy) emit(event Event) {
	select {
	case p.events <- event:
	default:
		p.dropped++
	}
}

// forwardBytes snapshots the cut before writing without holding the proxy mutex
// across I/O. A fault replacement cannot be blocked by a slow destination.
func (p *Proxy) forwardBytes(connection *link, direction Direction, dst net.Conn, data []byte) error {
	for len(data) > 0 {
		if err := p.wait(connection.ctx, true); err != nil {
			return err
		}
		p.mu.Lock()
		if connection.closed {
			p.mu.Unlock()
			return net.ErrClosed
		}
		fault, generation := p.fault, p.generation
		limited := fault.CutAfter > 0 && fault.CutDirection == direction
		part := data
		if limited {
			remaining := fault.CutAfter - connection.forwarded[direction-1]
			if int64(len(part)) > remaining {
				part = part[:remaining]
			}
		}
		p.mu.Unlock()

		written, err := dst.Write(part)
		p.mu.Lock()
		cut := false
		if generation == p.generation && !connection.closed {
			connection.forwarded[direction-1] += int64(written)
			cut = limited && connection.forwarded[direction-1] == fault.CutAfter
		}
		if written > 0 || cut {
			p.emit(Event{Connection: connection.id, Direction: direction, Bytes: int64(written), Cut: cut})
		}
		if cut {
			p.recordClose(p.closeLink(connection))
		}
		p.mu.Unlock()
		if err != nil {
			return err
		}
		if cut {
			return net.ErrClosed
		}
		if written != len(part) {
			return io.ErrShortWrite
		}
		data = data[written:]
	}
	return nil
}
