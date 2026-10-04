// SPDX-License-Identifier: AGPL-3.0-only
package netfault

import (
	"io"
	"net"
)

// forwardBytes snapshots the cut before writing without holding the proxy mutex
// across I/O. A fault replacement cannot be blocked by a slow destination.
func (p *Proxy) forwardBytes(connection *link, direction Direction, dst net.Conn, data []byte) error {
	p.mu.Lock()
	if connection.closed {
		p.mu.Unlock()
		return net.ErrClosed
	}
	fault, generation := p.fault, p.generation
	limited := fault.CutAfter > 0 && fault.CutDirection == direction
	if limited {
		remaining := fault.CutAfter - connection.forwarded[direction-1]
		if int64(len(data)) > remaining {
			data = data[:remaining]
		}
	}
	p.mu.Unlock()

	written, err := dst.Write(data)
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
	if written != len(data) {
		return io.ErrShortWrite
	}
	return nil
}
