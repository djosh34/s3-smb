// SPDX-License-Identifier: AGPL-3.0-only

package chaos

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/djosh34/s3-smb/internal/netfault"
	"github.com/djosh34/s3-smb/internal/s3fault"
)

// Step changes faults at an offset from the start of Run. Nil fault pointers
// leave that proxy alone; pointers to zero faults restore normal forwarding.
// Cut closes current network connections. S3Outage replaces the outage deadline
// when positive; zero leaves any existing outage alone.
//
//nolint:govet // Field order is the shared scenario contract in issue #349.
type Step struct {
	At       time.Duration
	Net      *netfault.Fault
	Cut      bool
	S3       *s3fault.Fault
	S3Outage time.Duration
}

// Schedule is a list of faults in nondecreasing offset order. Equal offsets run
// in slice order. Build it with Rand for scenario-specific fault distributions,
// or use Generate for a small mixed schedule.
type Schedule []Step

// Generate builds count fault steps, spaced interval apart, followed by a
// zero-fault step. It mixes network delay, stalls and drops with S3 errors,
// throttling, delay and outages. Outages end on their own deadlines. The same
// seed, count and interval replay it.
func Generate(seed uint64, count int, interval time.Duration) (Schedule, error) {
	if count < 1 || count == math.MaxInt || interval <= 0 || int64(count) > math.MaxInt64/int64(interval) {
		return nil, errors.New("schedule needs a positive count and interval without duration overflow")
	}
	random := Rand(seed, "schedule")
	steps := make(Schedule, 0, count+1)
	for i := range count {
		network := &netfault.Fault{}
		s3 := &s3fault.Fault{}
		step := Step{At: time.Duration(i) * interval, Net: network, S3: s3}
		switch random.IntN(4) {
		case 0:
			network.Delay = interval / 8
		case 1:
			network.Stall = true
		case 2:
			step.Cut = true
		case 3:
			network.Drop = true
		}
		switch random.IntN(4) {
		case 0:
			s3.HeaderDelay = interval / 4
		case 1:
			s3.Status = 503
			s3.Code = "SlowDown"
		case 2:
			s3.BodyDelay = interval / 8
		case 3:
			step.S3Outage = interval / 2
		}
		steps = append(steps, step)
	}
	return append(steps, Step{At: time.Duration(count) * interval, Net: &netfault.Fault{}, S3: &s3fault.Fault{}}), nil
}

// String describes the ordered schedule, including all fault fields, for logs.
func (s Schedule) String() string {
	lines := make([]string, len(s))
	for i, step := range s {
		network, s3 := "unchanged", "unchanged"
		if step.Net != nil {
			network = fmt.Sprintf("%+v", *step.Net)
		}
		if step.S3 != nil {
			s3 = fmt.Sprintf("%+v", *step.S3)
		}
		lines[i] = fmt.Sprintf("at=%s net=%s cut=%t s3=%s outage=%s", step.At, network, step.Cut, s3, step.S3Outage)
	}
	return strings.Join(lines, "\n")
}

// Run applies each step at its offset until the schedule or ctx ends. It
// snapshots the steps before waiting. Do not mutate the schedule while Run
// takes that snapshot. Faults stay in place on completion, cancellation or error;
// a scenario owns restoration and must wait for Run before closing its proxies.
func (s Schedule) Run(ctx context.Context, network *netfault.Proxy, s3 *s3fault.Proxy) error {
	steps, err := s.prepare(network, s3)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	start := time.Now()
	for i, step := range steps {
		if err := waitUntil(ctx, start.Add(step.At)); err != nil {
			return err
		}
		if err := step.apply(network, s3); err != nil {
			return fmt.Errorf("schedule step %d at %s: %w", i, step.At, err)
		}
	}
	return nil
}

func (s Schedule) prepare(network *netfault.Proxy, s3 *s3fault.Proxy) (Schedule, error) {
	steps := make(Schedule, len(s))
	var previous time.Duration
	for i, step := range s {
		if step.At < previous || step.S3Outage < 0 {
			return nil, fmt.Errorf("schedule step %d: negative or unordered time", i)
		}
		if (step.Net != nil || step.Cut) && network == nil {
			return nil, fmt.Errorf("schedule step %d: network proxy required", i)
		}
		if (step.S3 != nil || step.S3Outage != 0) && s3 == nil {
			return nil, fmt.Errorf("schedule step %d: S3 proxy required", i)
		}
		if step.Net != nil {
			fault := *step.Net
			if fault.Delay < 0 {
				return nil, fmt.Errorf("schedule step %d: negative network delay", i)
			}
			step.Net = &fault
		}
		if step.S3 != nil {
			fault := *step.S3
			if fault.HeaderDelay < 0 || fault.BodyDelay < 0 || fault.CutAfter < 0 || (fault.Status != 0 && (fault.Status < 400 || fault.Status > 599)) {
				return nil, fmt.Errorf("schedule step %d: invalid S3 fault", i)
			}
			step.S3 = &fault
		}
		steps[i] = step
		previous = step.At
	}
	return steps, nil
}

func waitUntil(ctx context.Context, deadline time.Time) error {
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return ctx.Err() // A due timer must not beat an already canceled context.
	}
}

func (s Step) apply(network *netfault.Proxy, s3 *s3fault.Proxy) error {
	if s.Net != nil {
		if err := network.SetFault(*s.Net); err != nil {
			return fmt.Errorf("set network fault: %w", err)
		}
	}
	if s.Cut {
		if err := network.Cut(); err != nil {
			return fmt.Errorf("cut network connections: %w", err)
		}
	}
	if s.S3 != nil {
		if err := s3.SetFault(*s.S3); err != nil {
			return fmt.Errorf("set S3 fault: %w", err)
		}
	}
	if s.S3Outage > 0 {
		s3.FailS3For(s.S3Outage)
	}
	return nil
}
