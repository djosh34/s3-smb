// SPDX-License-Identifier: AGPL-3.0-only

package netfault

import (
	"context"
	"net"
	"time"
)

// bandwidthReady also checks the generation after the wait. A replacement
// racing with a due timer must be observed before starting the buffer's write.
func (p *Proxy) bandwidthReady(connection *link, generation uint64, changed <-chan struct{}, size int, rate int64) (bool, error) {
	ready, err := waitBandwidth(connection.ctx, changed, size, rate)
	if err != nil {
		return false, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if connection.closed {
		return false, net.ErrClosed
	}
	return ready && generation == p.generation, nil
}

// waitBandwidth charges a buffer before forwarding it. A relay sends one buffer
// at a time, so no state or shared budget is needed. size is at most 32 KiB.
// A replacement returns false with the buffer untouched, ready for a new fault.
func waitBandwidth(ctx context.Context, changed <-chan struct{}, size int, rate int64) (bool, error) {
	if rate == 0 || size == 0 {
		return true, ctx.Err()
	}
	// Round up so even caps above one billion bytes per second cannot send
	// without charging time. The bounded buffer keeps the multiplication safe.
	delay := time.Duration((int64(size)*int64(time.Second)-1)/rate + 1)
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false, ctx.Err()
	case <-changed:
		return false, nil
	case <-timer.C:
		return true, ctx.Err()
	}
}
