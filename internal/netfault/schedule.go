// SPDX-License-Identifier: AGPL-3.0-only

package netfault

import (
	"context"
	"errors"
	"net"
	"time"
)

// Schedule runs a copied sequence of steps in nondecreasing After order. Only
// one schedule can run at a time. The buffered result receives one completion
// error (nil on success), then closes. Canceling ctx stops future steps but
// leaves the last fault in place. Closing the proxy stops the schedule too.
func (p *Proxy) Schedule(ctx context.Context, steps []Step) (<-chan error, error) {
	if len(steps) == 0 {
		return nil, errors.New("empty network fault schedule")
	}
	var previous time.Duration
	for _, step := range steps {
		if step.After < previous || step.Fault.Delay < 0 {
			return nil, errors.New("invalid network fault schedule time or delay")
		}
		previous = step.After
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, net.ErrClosed
	}
	if p.scheduled {
		return nil, errors.New("network fault schedule already running")
	}
	p.scheduled = true
	p.workers.Add(1)
	copied := append([]Step(nil), steps...)
	start := time.Now()
	done := make(chan error, 1)
	go func() {
		defer p.workers.Done()
		err := p.runSteps(ctx, start, copied)
		p.mu.Lock()
		p.scheduled = false
		p.mu.Unlock()
		done <- err
		close(done)
	}()
	return done, nil
}

func (p *Proxy) runSteps(ctx context.Context, start time.Time, steps []Step) error {
	for _, step := range steps {
		timer := time.NewTimer(time.Until(start.Add(step.After)))
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-p.ctx.Done():
			timer.Stop()
			return net.ErrClosed
		case <-timer.C:
		}
		p.mu.Lock()
		// Check again when a due timer races with cancellation.
		err := ctx.Err()
		if err == nil {
			err = p.apply(step.Fault, step.Cut)
		}
		p.mu.Unlock()
		if err != nil {
			return err
		}
	}
	return nil
}
