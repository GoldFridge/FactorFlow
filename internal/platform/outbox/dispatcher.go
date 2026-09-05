package outbox

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/skimer2king/factorflow/internal/platform/postgres"
)

// Handler performs the external effect an event describes.
//
// A handler must be idempotent: delivery is at-least-once, and a handler that already did
// its work must return nil rather than repeating a chain transaction or a payment.
type Handler func(ctx context.Context, event Event) error

// Backoff describes how long to wait after a failed delivery.
type Backoff struct {
	// Base is the first delay; each further attempt doubles it.
	Base time.Duration
	// Max caps the delay however many attempts have failed.
	Max time.Duration
	// Jitter is the fraction of the delay that is randomized, spreading retries so a
	// recovering dependency is not hit by every worker at the same instant.
	Jitter float64
	// Rand supplies the jitter. Tests set it to a fixed source so a retry schedule is
	// reproducible; production leaves it nil and gets the global source.
	Rand *rand.Rand
}

// DefaultBackoff matches the specification's retry policy for confidential and chain work:
// a few quick attempts, then space them out.
func DefaultBackoff() Backoff {
	return Backoff{Base: 2 * time.Second, Max: 5 * time.Minute, Jitter: 0.2}
}

// Delay returns how long to wait before attempt number attempts+1.
func (b Backoff) Delay(attempts int) time.Duration {
	if attempts < 0 {
		attempts = 0
	}
	delay := b.Base
	for range attempts {
		delay *= 2
		if delay >= b.Max {
			delay = b.Max
			break
		}
	}

	if b.Jitter <= 0 {
		return delay
	}
	spread := float64(delay) * b.Jitter
	offset := b.random()*2*spread - spread
	jittered := time.Duration(float64(delay) + offset)
	if jittered < 0 {
		return 0
	}
	return jittered
}

func (b Backoff) random() float64 {
	if b.Rand != nil {
		return b.Rand.Float64()
	}
	return rand.Float64()
}

// DispatcherConfig configures a worker.
type DispatcherConfig struct {
	// BatchSize is how many events one pass claims.
	BatchSize int
	// MaxAttempts is how often an event is retried before it is parked for an operator.
	// Parking beats retrying forever: an event failing for a reason no retry can fix
	// would otherwise fill the log and hide the events that can still succeed.
	MaxAttempts int
	// ParkFor is how long a parked event waits before it would be retried automatically.
	ParkFor time.Duration
	Backoff Backoff
}

// DefaultDispatcherConfig returns the worker settings used by the single-server demo.
func DefaultDispatcherConfig() DispatcherConfig {
	return DispatcherConfig{
		BatchSize:   50,
		MaxAttempts: 3,
		ParkFor:     24 * time.Hour,
		Backoff:     DefaultBackoff(),
	}
}

// Dispatcher delivers outbox events to their handlers.
type Dispatcher struct {
	db       *postgres.DB
	store    *Store
	handlers map[string]Handler
	config   DispatcherConfig
	now      func() time.Time
}

// NewDispatcher wires a dispatcher. The clock is injected so tests can advance time
// instead of sleeping through a backoff.
func NewDispatcher(db *postgres.DB, config DispatcherConfig, now func() time.Time) *Dispatcher {
	if now == nil {
		now = time.Now
	}
	if config.BatchSize <= 0 {
		config.BatchSize = DefaultDispatcherConfig().BatchSize
	}
	if config.MaxAttempts <= 0 {
		config.MaxAttempts = DefaultDispatcherConfig().MaxAttempts
	}
	if config.ParkFor <= 0 {
		config.ParkFor = DefaultDispatcherConfig().ParkFor
	}

	return &Dispatcher{
		db:       db,
		store:    NewStore(),
		handlers: map[string]Handler{},
		config:   config,
		now:      now,
	}
}

// Register attaches a handler to a topic. Registering a topic twice is a wiring mistake and
// panics at startup rather than silently dropping the first handler.
func (d *Dispatcher) Register(topic string, handler Handler) {
	if _, exists := d.handlers[topic]; exists {
		panic(fmt.Sprintf("outbox: topic %q already has a handler", topic))
	}
	d.handlers[topic] = handler
}

// Result reports what one pass did.
type Result struct {
	Claimed   int
	Delivered int
	Failed    int
	Parked    int
}

// RunOnce claims a batch and delivers it, returning what happened.
//
// Each event is handled in its own transaction. One poisoned event therefore cannot roll
// back the successful deliveries around it, which is what keeps a single broken chain call
// from stalling the whole queue.
func (d *Dispatcher) RunOnce(ctx context.Context) (Result, error) {
	var claimed []Event
	err := d.db.InTx(ctx, func(q postgres.Querier) error {
		events, err := d.store.FetchDue(ctx, q, d.config.BatchSize, d.now())
		if err != nil {
			return err
		}
		claimed = events
		return nil
	})
	if err != nil {
		return Result{}, err
	}

	result := Result{Claimed: len(claimed)}
	for _, event := range claimed {
		if err := ctx.Err(); err != nil {
			return result, err
		}

		handler, ok := d.handlers[event.Topic]
		if !ok {
			d.recordFailure(ctx, event, fmt.Errorf("%w: %s", ErrUnhandledTopic, event.Topic), &result)
			continue
		}

		if err := handler(ctx, event); err != nil {
			d.recordFailure(ctx, event, err, &result)
			continue
		}

		if err := d.db.InTx(ctx, func(q postgres.Querier) error {
			return d.store.MarkDelivered(ctx, q, event.ID, d.now())
		}); err != nil {
			return result, err
		}
		result.Delivered++
	}

	return result, nil
}

// recordFailure reschedules or parks a failed event. A failure to write that outcome is
// itself only logged into the row on the next pass: the worker keeps going rather than
// stopping the whole queue for one row.
func (d *Dispatcher) recordFailure(ctx context.Context, event Event, cause error, result *Result) {
	attempts := event.Attempts + 1
	delay := d.config.Backoff.Delay(event.Attempts)
	if attempts >= d.config.MaxAttempts {
		delay = d.config.ParkFor
		result.Parked++
	}
	result.Failed++

	reschedule := d.db.InTx(ctx, func(q postgres.Querier) error {
		return d.store.Reschedule(ctx, q, event.ID, cause.Error(), d.now().Add(delay))
	})
	if reschedule != nil && !errors.Is(reschedule, context.Canceled) {
		// Nothing else to do: the event stays due and is retried on the next pass.
		_ = reschedule
	}
}

// Run polls until the context is cancelled.
func (d *Dispatcher) Run(ctx context.Context, interval time.Duration) error {
	if interval <= 0 {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if _, err := d.RunOnce(ctx); err != nil {
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					return err
				}
				// A database hiccup must not kill the worker; the next tick retries.
				continue
			}
		}
	}
}
