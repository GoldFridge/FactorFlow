// Package clock is the source of time the services read.
//
// Almost everything wants the real clock, and that is what Live returns. The reason the
// package exists is the other case: building a dataset with a history in it. A batch that
// has already closed and been paid cannot be produced by services that think everything
// happens now, because the rules refuse to close a window that has not ended.
//
// The alternative would be to special-case those rules for seeded data, which would mean
// the seeded data no longer demonstrates the rules. Moving the clock instead keeps every
// transition subject to the same checks a live one passes.
package clock

import (
	"sync"
	"time"
)

// Clock hands out the current instant.
//
// A live clock follows the wall clock and ignores attempts to move it. A fixed one starts
// where it was told and moves only when asked, so a caller can build a sequence of events
// that really did happen in that order.
type Clock struct {
	mu    sync.Mutex
	fixed bool
	at    time.Time
}

// Live returns a clock that follows real time. Advance and Set do nothing to it: a running
// service must not be able to change what time it is.
func Live() *Clock { return &Clock{} }

// At returns a clock stopped at an instant.
func At(t time.Time) *Clock { return &Clock{fixed: true, at: t.UTC()} }

// Now returns the current instant.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.fixed {
		return time.Now()
	}
	return c.at
}

// IsFixed reports whether this clock can be moved.
func (c *Clock) IsFixed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.fixed
}

// Advance moves a fixed clock forward.
//
// It never moves backwards: a history that went back on itself would produce timestamps no
// timeline could order, and the audit trail is ordered by them.
func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.fixed && d > 0 {
		c.at = c.at.Add(d)
	}
}

// Set moves a fixed clock to an instant, ignoring an instant already past. It is how a
// caller that has been building history returns to the present.
func (c *Clock) Set(at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.fixed && at.After(c.at) {
		c.at = at.UTC()
	}
}
