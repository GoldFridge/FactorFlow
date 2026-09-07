// Package outbox implements the transactional outbox that keeps external side effects
// consistent with the state that caused them.
//
// The problem it solves is specific. PostgreSQL and Hedera cannot share a transaction, so
// a naive service either writes its state and then fails before the chain call, or calls
// the chain and then fails before recording it. Writing an intent row inside the same
// transaction as the state change removes that gap: if the state is committed the intent
// is committed with it, and a worker delivers it afterwards, retrying until it succeeds.
//
// Delivery is at-least-once, so every handler must be idempotent. That is why the domain
// commands carry idempotency keys and why chain adapters check for an existing receipt
// before submitting a second transaction.
package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/GoldFridge/factorflow/internal/platform/apperr"
	"github.com/GoldFridge/factorflow/internal/platform/postgres"
)

// Event is one durable intent to perform an external effect.
type Event struct {
	ID       int64
	Topic    string
	Payload  json.RawMessage
	Attempts int
	// LastError is the most recent delivery failure, kept for the operator view. It must
	// never carry document content or secrets.
	LastError string
	TraceID   string
	// AvailableAt is when the event may next be attempted.
	AvailableAt time.Time
	DeliveredAt *time.Time
	CreatedAt   time.Time
}

// IsDelivered reports whether the event has already been handled.
func (e *Event) IsDelivered() bool { return e.DeliveredAt != nil }

// Publish writes an event inside the caller's transaction.
//
// It takes a Querier, not a pool, precisely so it cannot be called outside one by
// accident: an outbox row committed separately from its state change would reintroduce the
// gap the pattern exists to close.
func Publish(ctx context.Context, q postgres.Querier, topic string, payload any, traceID string, now time.Time) error {
	if topic == "" {
		return apperr.Invalid("topic", "must not be empty")
	}

	encoded, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encoding outbox payload for %s: %w", topic, err)
	}

	const query = `
		INSERT INTO outbox_events (topic, payload, trace_id, available_at, created_at)
		VALUES ($1, $2, $3, $4, $4)`

	_, err = q.Exec(ctx, query, topic, encoded, traceID, now.UTC())
	return postgres.Translate(err)
}

// Store reads and updates outbox rows.
type Store struct{}

// NewStore returns the store.
func NewStore() *Store { return &Store{} }

// FetchDue claims up to limit events that are ready for delivery.
//
// The rows are locked with SKIP LOCKED, so several workers, or several instances during a
// rolling restart, never pick up the same event. The claim only holds inside the caller's
// transaction, which is why delivery marks its result in that same transaction.
func (s *Store) FetchDue(ctx context.Context, q postgres.Querier, limit int, now time.Time) ([]Event, error) {
	const query = `
		SELECT id, topic, payload, attempts, last_error, trace_id, available_at, delivered_at, created_at
		  FROM outbox_events
		 WHERE delivered_at IS NULL AND available_at <= $1
		 ORDER BY available_at, id
		 LIMIT $2
		   FOR UPDATE SKIP LOCKED`

	if limit <= 0 || limit > 500 {
		limit = 50
	}

	rows, err := q.Query(ctx, query, now.UTC(), limit)
	if err != nil {
		return nil, postgres.Translate(err)
	}
	defer rows.Close()

	var events []Event
	for rows.Next() {
		var (
			event       Event
			availableAt time.Time
			createdAt   time.Time
			deliveredAt *time.Time
		)
		if err := rows.Scan(&event.ID, &event.Topic, &event.Payload, &event.Attempts,
			&event.LastError, &event.TraceID, &availableAt, &deliveredAt, &createdAt); err != nil {
			return nil, postgres.Translate(err)
		}
		event.AvailableAt = availableAt.UTC()
		event.CreatedAt = createdAt.UTC()
		if deliveredAt != nil {
			utc := deliveredAt.UTC()
			event.DeliveredAt = &utc
		}
		events = append(events, event)
	}
	return events, postgres.Translate(rows.Err())
}

// MarkDelivered records a successful delivery.
func (s *Store) MarkDelivered(ctx context.Context, q postgres.Querier, id int64, now time.Time) error {
	const query = `
		UPDATE outbox_events
		   SET delivered_at = $2, attempts = attempts + 1, last_error = ''
		 WHERE id = $1 AND delivered_at IS NULL`

	tag, err := q.Exec(ctx, query, id, now.UTC())
	if err != nil {
		return postgres.Translate(err)
	}
	if tag.RowsAffected() == 0 {
		return apperr.Conflictf("outbox event %d is already delivered", id)
	}
	return nil
}

// Reschedule records a failed delivery and when to try again.
func (s *Store) Reschedule(ctx context.Context, q postgres.Querier, id int64, reason string, nextAt time.Time) error {
	const query = `
		UPDATE outbox_events
		   SET attempts = attempts + 1, last_error = $2, available_at = $3
		 WHERE id = $1 AND delivered_at IS NULL`

	_, err := q.Exec(ctx, query, id, truncateError(reason), nextAt.UTC())
	return postgres.Translate(err)
}

// Retry makes a parked event available again. It backs the operator's "replay this outbox
// task" action, which the specification requires for a failed external call.
func (s *Store) Retry(ctx context.Context, q postgres.Querier, id int64, now time.Time) error {
	const query = `
		UPDATE outbox_events
		   SET available_at = $2, attempts = 0, last_error = ''
		 WHERE id = $1 AND delivered_at IS NULL`

	tag, err := q.Exec(ctx, query, id, now.UTC())
	if err != nil {
		return postgres.Translate(err)
	}
	if tag.RowsAffected() == 0 {
		return apperr.NotFoundf("undelivered outbox event %d", id)
	}
	return nil
}

// ListParked returns events that exhausted their attempts and are waiting for an operator.
func (s *Store) ListParked(ctx context.Context, q postgres.Querier, minAttempts, limit int) ([]Event, error) {
	const query = `
		SELECT id, topic, payload, attempts, last_error, trace_id, available_at, delivered_at, created_at
		  FROM outbox_events
		 WHERE delivered_at IS NULL AND attempts >= $1
		 ORDER BY id
		 LIMIT $2`

	if limit <= 0 || limit > 500 {
		limit = 50
	}

	rows, err := q.Query(ctx, query, minAttempts, limit)
	if err != nil {
		return nil, postgres.Translate(err)
	}
	defer rows.Close()

	var events []Event
	for rows.Next() {
		var (
			event       Event
			availableAt time.Time
			createdAt   time.Time
			deliveredAt *time.Time
		)
		if err := rows.Scan(&event.ID, &event.Topic, &event.Payload, &event.Attempts,
			&event.LastError, &event.TraceID, &availableAt, &deliveredAt, &createdAt); err != nil {
			return nil, postgres.Translate(err)
		}
		event.AvailableAt = availableAt.UTC()
		event.CreatedAt = createdAt.UTC()
		events = append(events, event)
	}
	return events, postgres.Translate(rows.Err())
}

// maxErrorLen bounds what a failure writes into the row: telemetry must stay redacted, and
// an unbounded driver error would be neither readable nor safe.
const maxErrorLen = 500

func truncateError(reason string) string {
	if len(reason) <= maxErrorLen {
		return reason
	}
	return reason[:maxErrorLen] + "..."
}

// ErrUnhandledTopic reports an event whose topic has no registered handler.
var ErrUnhandledTopic = errors.New("outbox: no handler registered for topic")
