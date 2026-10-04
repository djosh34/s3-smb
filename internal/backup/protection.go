// SPDX-License-Identifier: AGPL-3.0-only
package backup

import (
	"errors"
	"sync"
	"time"
)

// ErrUnprotected reports that no recent verified metadata backup exists, so
// data must not be deleted.
var ErrUnprotected = errors.New("metadata protection is absent or expired")

// Protection allows deleting data only while a recent metadata backup exists.
// JuiceFS cleanup and the object delete guard both call Check. Check compares
// wall time, so protection that expired during a host suspend stays expired. It
// starts closed and opens at the first verified backup.
type Protection struct {
	now              func() time.Time
	snapshot         time.Time
	interval, budget time.Duration
	mu               sync.RWMutex
	stopped          bool
}

// NewProtection returns closed protection for backups every interval that may
// take up to budget. Their sum must stay below the JuiceFS trash retention.
func NewProtection(interval, budget time.Duration, trashDays int) (*Protection, error) {
	if interval <= 0 || budget <= 0 || trashDays <= 0 || interval > time.Duration(1<<63-1)-budget {
		return nil, errors.New("invalid backup interval, budget or trash retention")
	}
	// JuiceFS computes trash expiry as time.Duration(24*days+2)*time.Hour.
	// More days than this overflow that duration.
	const maxTrashDays = int((time.Duration(1<<63-1)/time.Hour - 2) / 24)
	if trashDays > maxTrashDays {
		return nil, errors.New("backup.trash_days exceeds the JuiceFS limit of 106751 days")
	}
	if interval+budget >= time.Duration(trashDays)*24*time.Hour {
		return nil, errors.New("backup interval plus the time allowed for one backup must be shorter than trash retention")
	}
	return &Protection{interval: interval, budget: budget, now: time.Now}, nil
}

// Check returns ErrUnprotected unless the last verified backup is recent.
func (p *Protection) Check() error {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.stopped || p.snapshot.IsZero() || !p.covers(p.snapshot, p.now().Round(0)) {
		return ErrUnprotected
	}
	return nil
}

// Close ends protection for the rest of the process. A backup that finishes
// later does not reopen it.
func (p *Protection) Close() { p.mu.Lock(); p.stopped = true; p.mu.Unlock() }

func (p *Protection) protect(snapshot time.Time) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now().Round(0)
	snapshot = snapshot.Round(0)
	// A late backup must not revive a window that already expired.
	if p.stopped || !p.covers(snapshot, now) || (!p.snapshot.IsZero() && !p.covers(p.snapshot, now)) {
		return ErrUnprotected
	}
	p.snapshot = snapshot
	return nil
}

// covers reports whether a backup started at snapshot protects time now.
func (p *Protection) covers(snapshot, now time.Time) bool {
	return !now.Before(snapshot) && now.Before(snapshot.Add(p.interval+p.budget))
}
