package sim

import (
	"context"
	"time"
)

// Pacer maps virtual time onto wall-clock time at a speed factor. It is the
// only place in the package that touches timers; with Speed <= 0 it never
// does, which is what tests use.
type Pacer struct {
	start time.Time // virtual t0
	speed float64
	began time.Time // wall clock when Wait was first called

	// Injected for tests; nil means the real clock.
	now   func() time.Time
	after func(d time.Duration) <-chan time.Time
}

// NewPacer returns a Pacer for a run starting at virtual time start.
func NewPacer(start time.Time, speed float64) *Pacer {
	return &Pacer{start: start, speed: speed}
}

// Wait blocks until wall-clock time has caught up with virtualNow, or ctx is
// cancelled. It returns immediately when the run is unpaced.
func (p *Pacer) Wait(ctx context.Context, virtualNow time.Time) error {
	if p.speed <= 0 {
		return nil
	}
	now := time.Now
	if p.now != nil {
		now = p.now
	}
	if p.began.IsZero() {
		p.began = now()
	}
	target := p.began.Add(time.Duration(float64(virtualNow.Sub(p.start)) / p.speed))
	d := target.Sub(now())
	if d <= 0 {
		return nil
	}
	after := time.After
	if p.after != nil {
		after = p.after
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-after(d):
		return nil
	}
}
