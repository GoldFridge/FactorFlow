// Package audittest collects audit events in memory so a test can assert what a service
// recorded.
//
// It validates every event the way the real recorder does. A fake that accepted anything
// would let a malformed entry — no actor, no subject — pass the tests and fail only once
// the timeline was being read for real.
package audittest

import (
	"context"
	"sync"

	"github.com/GoldFridge/factorflow/internal/platform/audit"
	"github.com/GoldFridge/factorflow/internal/platform/postgres"
)

// Recorder is an in-memory audit.Recorder and audit.Reader.
type Recorder struct {
	mu     sync.Mutex
	events []audit.Event

	// failWith makes the next recording fail, which is how a test proves that a write path
	// aborts rather than committing a change it could not record.
	failWith error
}

// New returns an empty recorder.
func New() *Recorder { return &Recorder{} }

// Record appends an event.
func (r *Recorder) Record(_ context.Context, _ postgres.Querier, e audit.Event) error {
	if err := e.Validate(); err != nil {
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.failWith != nil {
		return r.failWith
	}
	r.events = append(r.events, e)
	return nil
}

// Timeline returns one entity's events, newest first, like the real reader.
func (r *Recorder) Timeline(_ context.Context, _ postgres.Querier, entityType, entityID string, limit int) ([]audit.Event, error) {
	events := r.For(entityType, entityID)
	if limit > 0 && len(events) > limit {
		events = events[:limit]
	}
	return events, nil
}

// FailWith makes every later recording return err.
func (r *Recorder) FailWith(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.failWith = err
}

// Events returns everything recorded, in the order it was recorded.
func (r *Recorder) Events() []audit.Event {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]audit.Event, len(r.events))
	copy(out, r.events)
	return out
}

// For returns one entity's events, newest first.
func (r *Recorder) For(entityType, entityID string) []audit.Event {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]audit.Event, 0, len(r.events))
	for i := len(r.events) - 1; i >= 0; i-- {
		if r.events[i].EntityType == entityType && r.events[i].EntityID == entityID {
			out = append(out, r.events[i])
		}
	}
	return out
}

// Actions returns the recorded actions in order, which is usually the whole assertion.
func (r *Recorder) Actions() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]string, 0, len(r.events))
	for _, e := range r.events {
		out = append(out, e.Action)
	}
	return out
}

// Last returns the most recent event.
func (r *Recorder) Last() (audit.Event, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if len(r.events) == 0 {
		return audit.Event{}, false
	}
	return r.events[len(r.events)-1], true
}

// Reset forgets everything recorded so far.
func (r *Recorder) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.events = nil
}
