package outbox_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/skimer2king/factorflow/internal/platform/apperr"
	"github.com/skimer2king/factorflow/internal/platform/outbox"
	"github.com/skimer2king/factorflow/internal/platform/pgtest"
	"github.com/skimer2king/factorflow/internal/platform/postgres"
)

var testNow = time.Date(2026, time.September, 5, 12, 0, 0, 0, time.UTC)

func TestMain(m *testing.M) {
	code := m.Run()
	pgtest.Shutdown()
	os.Exit(code)
}

type assessCommand struct {
	InvoiceID string `json:"invoice_id"`
	Attempt   int    `json:"attempt"`
}

func publish(t *testing.T, db *postgres.DB, topic string, payload any, at time.Time) {
	t.Helper()

	require.NoError(t, db.InTx(context.Background(), func(q postgres.Querier) error {
		return outbox.Publish(context.Background(), q, topic, payload, "trace-1", at)
	}))
}

func TestPublishAndFetch(t *testing.T) {
	db := pgtest.New(t)
	ctx := context.Background()
	store := outbox.NewStore()

	publish(t, db, "invoice.assess", assessCommand{InvoiceID: "abc", Attempt: 1}, testNow)

	var events []outbox.Event
	require.NoError(t, db.InTx(ctx, func(q postgres.Querier) error {
		var err error
		events, err = store.FetchDue(ctx, q, 10, testNow)
		return err
	}))

	require.Len(t, events, 1)
	assert.Equal(t, "invoice.assess", events[0].Topic)
	assert.Equal(t, 0, events[0].Attempts)
	assert.Equal(t, "trace-1", events[0].TraceID)
	assert.False(t, events[0].IsDelivered())

	var decoded assessCommand
	require.NoError(t, json.Unmarshal(events[0].Payload, &decoded))
	assert.Equal(t, "abc", decoded.InvoiceID)
}

// TestPublishJoinsTheCallersTransaction is the whole point of the pattern: if the state
// change rolls back, the intent to act on it rolls back with it.
func TestPublishJoinsTheCallersTransaction(t *testing.T) {
	db := pgtest.New(t)
	ctx := context.Background()
	store := outbox.NewStore()

	wantErr := errors.New("domain rejected the command")
	err := db.InTx(ctx, func(q postgres.Querier) error {
		if err := outbox.Publish(ctx, q, "invoice.assess", assessCommand{InvoiceID: "abc"}, "", testNow); err != nil {
			return err
		}
		return wantErr
	})
	require.ErrorIs(t, err, wantErr)

	var events []outbox.Event
	require.NoError(t, db.InTx(ctx, func(q postgres.Querier) error {
		var err error
		events, err = store.FetchDue(ctx, q, 10, testNow)
		return err
	}))
	assert.Empty(t, events, "no state, no intent")
}

func TestPublishRejectsAnEmptyTopic(t *testing.T) {
	db := pgtest.New(t)
	ctx := context.Background()

	err := db.InTx(ctx, func(q postgres.Querier) error {
		return outbox.Publish(ctx, q, "", assessCommand{}, "", testNow)
	})
	require.ErrorIs(t, err, apperr.ErrValidation)
}

func TestFetchDueRespectsAvailability(t *testing.T) {
	db := pgtest.New(t)
	ctx := context.Background()
	store := outbox.NewStore()

	publish(t, db, "invoice.assess", assessCommand{InvoiceID: "now"}, testNow)
	publish(t, db, "invoice.assess", assessCommand{InvoiceID: "later"}, testNow.Add(time.Hour))

	var due []outbox.Event
	require.NoError(t, db.InTx(ctx, func(q postgres.Querier) error {
		var err error
		due, err = store.FetchDue(ctx, q, 10, testNow.Add(time.Minute))
		return err
	}))
	require.Len(t, due, 1, "an event scheduled for later is not claimed yet")

	require.NoError(t, db.InTx(ctx, func(q postgres.Querier) error {
		var err error
		due, err = store.FetchDue(ctx, q, 10, testNow.Add(2*time.Hour))
		return err
	}))
	assert.Len(t, due, 2)
}

func TestMarkDeliveredIsOnce(t *testing.T) {
	db := pgtest.New(t)
	ctx := context.Background()
	store := outbox.NewStore()

	publish(t, db, "invoice.assess", assessCommand{InvoiceID: "abc"}, testNow)

	var id int64
	require.NoError(t, db.InTx(ctx, func(q postgres.Querier) error {
		events, err := store.FetchDue(ctx, q, 10, testNow)
		if err != nil {
			return err
		}
		id = events[0].ID
		return store.MarkDelivered(ctx, q, id, testNow)
	}))

	err := db.InTx(ctx, func(q postgres.Querier) error {
		return store.MarkDelivered(ctx, q, id, testNow)
	})
	require.ErrorIs(t, err, apperr.ErrConflict, "a delivered event cannot be delivered again")

	var due []outbox.Event
	require.NoError(t, db.InTx(ctx, func(q postgres.Querier) error {
		var err error
		due, err = store.FetchDue(ctx, q, 10, testNow.Add(time.Hour))
		return err
	}))
	assert.Empty(t, due, "delivered events are never claimed again")
}

func TestRescheduleAndRetry(t *testing.T) {
	db := pgtest.New(t)
	ctx := context.Background()
	store := outbox.NewStore()

	publish(t, db, "invoice.assess", assessCommand{InvoiceID: "abc"}, testNow)

	var id int64
	require.NoError(t, db.InTx(ctx, func(q postgres.Querier) error {
		events, err := store.FetchDue(ctx, q, 10, testNow)
		if err != nil {
			return err
		}
		id = events[0].ID
		return store.Reschedule(ctx, q, id, "TEE timed out", testNow.Add(time.Hour))
	}))

	var due []outbox.Event
	require.NoError(t, db.InTx(ctx, func(q postgres.Querier) error {
		var err error
		due, err = store.FetchDue(ctx, q, 10, testNow.Add(time.Minute))
		return err
	}))
	assert.Empty(t, due, "a rescheduled event waits for its next slot")

	parked, err := store.ListParked(ctx, db.Querier(), 1, 10)
	require.NoError(t, err)
	require.Len(t, parked, 1)
	assert.Equal(t, 1, parked[0].Attempts)
	assert.Equal(t, "TEE timed out", parked[0].LastError)

	// The operator replays it, which is the action the specification requires.
	require.NoError(t, db.InTx(ctx, func(q postgres.Querier) error {
		return store.Retry(ctx, q, id, testNow.Add(2*time.Minute))
	}))

	require.NoError(t, db.InTx(ctx, func(q postgres.Querier) error {
		var err error
		due, err = store.FetchDue(ctx, q, 10, testNow.Add(2*time.Minute))
		return err
	}))
	require.Len(t, due, 1)
	assert.Equal(t, 0, due[0].Attempts, "a replay starts the attempt count over")
	assert.Empty(t, due[0].LastError)
}

func TestRetryUnknownEvent(t *testing.T) {
	db := pgtest.New(t)
	ctx := context.Background()

	err := db.InTx(ctx, func(q postgres.Querier) error {
		return outbox.NewStore().Retry(ctx, q, 99999, testNow)
	})
	require.ErrorIs(t, err, apperr.ErrNotFound)
}

func TestDispatcherDeliversAndRetries(t *testing.T) {
	db := pgtest.New(t)
	ctx := context.Background()

	clock := testNow
	dispatcher := outbox.NewDispatcher(db, outbox.DispatcherConfig{
		BatchSize:   10,
		MaxAttempts: 3,
		ParkFor:     24 * time.Hour,
		Backoff:     outbox.Backoff{Base: time.Minute, Max: time.Hour},
	}, func() time.Time { return clock })

	var seen []string
	failures := 1
	dispatcher.Register("invoice.assess", func(_ context.Context, event outbox.Event) error {
		var cmd assessCommand
		if err := json.Unmarshal(event.Payload, &cmd); err != nil {
			return err
		}
		if cmd.InvoiceID == "flaky" && failures > 0 {
			failures--
			return errors.New("TEE did not respond")
		}
		seen = append(seen, cmd.InvoiceID)
		return nil
	})

	publish(t, db, "invoice.assess", assessCommand{InvoiceID: "good"}, testNow)
	publish(t, db, "invoice.assess", assessCommand{InvoiceID: "flaky"}, testNow)

	result, err := dispatcher.RunOnce(ctx)
	require.NoError(t, err)
	assert.Equal(t, 2, result.Claimed)
	assert.Equal(t, 1, result.Delivered)
	assert.Equal(t, 1, result.Failed)
	assert.Equal(t, 0, result.Parked)
	assert.Equal(t, []string{"good"}, seen, "a failing event does not block the ones around it")

	// Before the backoff elapses the failed event is not retried.
	clock = testNow.Add(30 * time.Second)
	result, err = dispatcher.RunOnce(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, result.Claimed)

	clock = testNow.Add(2 * time.Minute)
	result, err = dispatcher.RunOnce(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, result.Delivered)
	assert.Equal(t, []string{"good", "flaky"}, seen)
}

func TestDispatcherParksAfterMaxAttempts(t *testing.T) {
	db := pgtest.New(t)
	ctx := context.Background()

	clock := testNow
	dispatcher := outbox.NewDispatcher(db, outbox.DispatcherConfig{
		BatchSize:   10,
		MaxAttempts: 3,
		ParkFor:     24 * time.Hour,
		Backoff:     outbox.Backoff{Base: time.Minute, Max: time.Minute},
	}, func() time.Time { return clock })

	dispatcher.Register("invoice.assess", func(context.Context, outbox.Event) error {
		return errors.New("permanent failure")
	})
	publish(t, db, "invoice.assess", assessCommand{InvoiceID: "doomed"}, testNow)

	for attempt := range 3 {
		clock = testNow.Add(time.Duration(attempt) * 5 * time.Minute)
		result, err := dispatcher.RunOnce(ctx)
		require.NoError(t, err)
		require.Equal(t, 1, result.Failed, "attempt %d", attempt)
	}

	parked, err := outbox.NewStore().ListParked(ctx, db.Querier(), 3, 10)
	require.NoError(t, err)
	require.Len(t, parked, 1)
	assert.Equal(t, 3, parked[0].Attempts)
	assert.Contains(t, parked[0].LastError, "permanent failure")

	// A parked event is out of the way until an operator replays it.
	clock = testNow.Add(time.Hour)
	result, err := dispatcher.RunOnce(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, result.Claimed)
}

func TestDispatcherHandlesUnknownTopic(t *testing.T) {
	db := pgtest.New(t)
	ctx := context.Background()

	dispatcher := outbox.NewDispatcher(db, outbox.DefaultDispatcherConfig(), func() time.Time { return testNow })
	publish(t, db, "invoice.unknown", assessCommand{InvoiceID: "abc"}, testNow)

	result, err := dispatcher.RunOnce(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, result.Failed)

	parked, err := outbox.NewStore().ListParked(ctx, db.Querier(), 1, 10)
	require.NoError(t, err)
	require.Len(t, parked, 1)
	assert.Contains(t, parked[0].LastError, "no handler registered")
}

func TestRegisteringATopicTwicePanics(t *testing.T) {
	t.Parallel()

	dispatcher := outbox.NewDispatcher(nil, outbox.DefaultDispatcherConfig(), nil)
	dispatcher.Register("invoice.assess", func(context.Context, outbox.Event) error { return nil })

	assert.Panics(t, func() {
		dispatcher.Register("invoice.assess", func(context.Context, outbox.Event) error { return nil })
	}, "a duplicate handler is a wiring mistake, not a runtime condition")
}

func TestBackoffGrowsAndIsCapped(t *testing.T) {
	t.Parallel()

	backoff := outbox.Backoff{Base: time.Second, Max: 8 * time.Second}

	assert.Equal(t, time.Second, backoff.Delay(0))
	assert.Equal(t, 2*time.Second, backoff.Delay(1))
	assert.Equal(t, 4*time.Second, backoff.Delay(2))
	assert.Equal(t, 8*time.Second, backoff.Delay(3))
	assert.Equal(t, 8*time.Second, backoff.Delay(10), "the delay is capped")
	assert.Equal(t, time.Second, backoff.Delay(-1), "a negative attempt count is treated as the first")
}

func TestBackoffJitterStaysInRange(t *testing.T) {
	t.Parallel()

	backoff := outbox.Backoff{
		Base:   time.Second,
		Max:    time.Minute,
		Jitter: 0.2,
		Rand:   rand.New(rand.NewPCG(1, 2)),
	}

	for attempt := range 5 {
		base := time.Duration(1<<attempt) * time.Second
		low := time.Duration(float64(base) * 0.8)
		high := time.Duration(float64(base) * 1.2)

		for range 20 {
			delay := backoff.Delay(attempt)
			require.GreaterOrEqualf(t, delay, low, "attempt %d", attempt)
			require.LessOrEqualf(t, delay, high, "attempt %d", attempt)
		}
	}
}

func TestBackoffJitterIsReproducible(t *testing.T) {
	t.Parallel()

	first := outbox.Backoff{Base: time.Second, Max: time.Minute, Jitter: 0.5, Rand: rand.New(rand.NewPCG(7, 9))}
	second := outbox.Backoff{Base: time.Second, Max: time.Minute, Jitter: 0.5, Rand: rand.New(rand.NewPCG(7, 9))}

	for attempt := range 10 {
		assert.Equalf(t, first.Delay(attempt), second.Delay(attempt), "attempt %d", attempt)
	}
}

func TestRunStopsWithTheContext(t *testing.T) {
	db := pgtest.New(t)

	dispatcher := outbox.NewDispatcher(db, outbox.DefaultDispatcherConfig(), nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := dispatcher.Run(ctx, 10*time.Millisecond)
	require.ErrorIs(t, err, context.Canceled)
}

// TestConcurrentWorkersDoNotDoubleDeliver covers the SKIP LOCKED claim: two workers
// draining the same queue must not both perform the same external effect.
func TestConcurrentWorkersDoNotDoubleDeliver(t *testing.T) {
	db := pgtest.New(t)
	ctx := context.Background()
	store := outbox.NewStore()

	for i := range 5 {
		publish(t, db, "invoice.assess", assessCommand{InvoiceID: fmt.Sprintf("inv-%d", i)}, testNow)
	}

	firstDone := make(chan []outbox.Event, 1)
	secondDone := make(chan []outbox.Event, 1)

	// The first worker holds its claim open while the second tries to claim as well.
	go func() {
		var claimed []outbox.Event
		_ = db.InTx(ctx, func(q postgres.Querier) error {
			events, err := store.FetchDue(ctx, q, 5, testNow)
			if err != nil {
				return err
			}
			claimed = events
			second := make(chan []outbox.Event, 1)
			go func() {
				var other []outbox.Event
				_ = db.InTx(ctx, func(q2 postgres.Querier) error {
					var err error
					other, err = store.FetchDue(ctx, q2, 5, testNow)
					return err
				})
				second <- other
			}()
			secondDone <- <-second
			return nil
		})
		firstDone <- claimed
	}()

	first := <-firstDone
	second := <-secondDone

	assert.Len(t, first, 5)
	assert.Empty(t, second, "locked rows are skipped, never handed to a second worker")
}
