// SPDX-License-Identifier: AGPL-3.0-only
package backup

import (
	"errors"
	"sync"
	"time"
)

var ErrUnprotected = errors.New("metadata protection is absent or expired")

// Protection is shared by native retirement and the final object deletion guard.
// It deliberately uses wall time (not a timer callback) so suspension cannot
// revive expired protection. The initial state is closed until a verified point.
type Protection struct {
	mu               sync.RWMutex
	interval, budget time.Duration
	snapshot         time.Time
	stopped          bool
	now              func() time.Time
}

func NewProtection(interval, budget time.Duration, trashDays int) (*Protection, error) {
	if interval <= 0 || budget <= 0 || trashDays <= 0 || interval >= time.Duration(trashDays)*24*time.Hour-budget {
		return nil, errors.New("backup interval plus operation budget must be shorter than native trash retention")
	}
	return &Protection{interval: interval, budget: budget, now: time.Now}, nil
}

func (p *Protection) Check() error {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := p.now().Round(0)
	if p.stopped || p.snapshot.IsZero() || now.Before(p.snapshot) || !now.Before(p.snapshot.Add(p.interval+p.budget)) {
		return ErrUnprotected
	}
	return nil
}

// Close is irreversible for this process. In-flight backups cannot reopen it.
func (p *Protection) Close() { p.mu.Lock(); p.stopped = true; p.mu.Unlock() }
func (p *Protection) protect(snapshot time.Time) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now().Round(0)
	snapshot = snapshot.Round(0)
	if p.stopped || snapshot.After(now) || !now.Before(snapshot.Add(p.interval+p.budget)) {
		return ErrUnprotected
	}
	p.snapshot = snapshot
	return nil
}
