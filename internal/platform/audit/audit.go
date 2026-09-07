// Package audit records what happened, as it happens, in the same transaction as the
// change itself.
//
// That is the whole point of writing it here rather than deriving a timeline from logs
// afterwards: a log line can be lost while the change lands, and a timeline that can
// disagree with the data is not evidence of anything. An audit row commits with the change
// or neither exists.
//
// The vocabulary is deliberately generic — an actor did an action to an entity — so this
// package stays underneath every module and can record any of them.
package audit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/GoldFridge/factorflow/internal/platform/apperr"
	"github.com/GoldFridge/factorflow/internal/platform/httpserver"
	"github.com/GoldFridge/factorflow/internal/platform/postgres"
)

// SystemActor is the actor recorded for work no person asked for directly: a background
// worker acting on an outbox event.
const SystemActor = "system"

// Event is one recorded change.
type Event struct {
	// Actor is who caused the change: an organization id, or SystemActor for a worker.
	Actor string
	// Action names what happened, as module.verb, for example "invoice.approved".
	Action string
	// EntityType and EntityID say what it happened to.
	EntityType string
	EntityID   string

	// BeforeHash and AfterHash are digests of the entity's relevant state on either side
	// of the change. They are hashes rather than copies because an audit trail must not
	// become a second, unencrypted copy of everything the platform holds.
	BeforeHash string
	AfterHash  string

	// TraceID ties the event to the request or the delivery that caused it.
	TraceID string
	// Detail carries the few facts a reader needs to understand the entry without opening
	// the record it describes. It must never carry document content.
	Detail map[string]any

	OccurredAt time.Time
}

// Validate checks that an event names its subject.
func (e Event) Validate() error {
	var violations []error

	if strings.TrimSpace(e.Actor) == "" {
		violations = append(violations, apperr.Invalid("actor", "must not be empty"))
	}
	if strings.TrimSpace(e.Action) == "" {
		violations = append(violations, apperr.Invalid("action", "must not be empty"))
	}
	if strings.TrimSpace(e.EntityType) == "" {
		violations = append(violations, apperr.Invalid("entity_type", "must not be empty"))
	}
	if strings.TrimSpace(e.EntityID) == "" {
		violations = append(violations, apperr.Invalid("entity_id", "must not be empty"))
	}
	if e.OccurredAt.IsZero() {
		violations = append(violations, apperr.Invalid("occurred_at", "must be set"))
	}

	return errors.Join(violations...)
}

// Recorder appends an event inside a caller's transaction.
type Recorder interface {
	Record(ctx context.Context, q postgres.Querier, e Event) error
}

// Reader returns what was recorded about one entity.
type Reader interface {
	Timeline(ctx context.Context, q postgres.Querier, entityType, entityID string, limit int) ([]Event, error)
}

// Hash digests the state an audit entry refers to.
//
// The value is marshalled as JSON first, so two callers describing the same state the same
// way produce the same digest, and a reader holding the state can check the entry against
// it. Anything that will not marshal is a programming mistake rather than a runtime
// condition, and is reported as an empty digest rather than a panic in a write path.
func Hash(state any) string {
	if state == nil {
		return ""
	}
	encoded, err := json.Marshal(state)
	if err != nil {
		return ""
	}
	// A typed nil — the usual "there was no prior state" argument — encodes as null. That
	// is absence, not a state worth digesting, so it hashes to nothing like an untyped nil.
	if string(encoded) == "null" {
		return ""
	}

	sum := sha256.Sum256(encoded)
	return "0x" + hex.EncodeToString(sum[:])
}

// Discard records nothing. It is the default for a service that was wired without an audit
// trail, so a missing recorder cannot panic a write path.
type Discard struct{}

// Record does nothing.
func (Discard) Record(context.Context, postgres.Querier, Event) error { return nil }

// PostgresRecorder writes and reads the audit table.
type PostgresRecorder struct{}

// NewPostgresRecorder returns the recorder.
func NewPostgresRecorder() *PostgresRecorder { return &PostgresRecorder{} }

const eventColumns = `actor, action, entity_type, entity_id, before_hash, after_hash, trace_id, detail, occurred_at`

// Record appends the event. It takes the caller's querier so the row lands in the same
// transaction as the change it describes.
func (r *PostgresRecorder) Record(ctx context.Context, q postgres.Querier, e Event) error {
	if err := e.Validate(); err != nil {
		return err
	}

	detail := e.Detail
	if detail == nil {
		detail = map[string]any{}
	}
	encoded, err := json.Marshal(detail)
	if err != nil {
		return fmt.Errorf("encoding audit detail: %w", err)
	}

	const query = `INSERT INTO audit_events (` + eventColumns + `)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`

	if _, err := q.Exec(ctx, query,
		e.Actor, e.Action, e.EntityType, e.EntityID,
		e.BeforeHash, e.AfterHash, e.TraceID, encoded, e.OccurredAt.UTC()); err != nil {
		return fmt.Errorf("recording audit event: %w", err)
	}
	return nil
}

// Timeline returns an entity's events, newest first.
func (r *PostgresRecorder) Timeline(ctx context.Context, q postgres.Querier, entityType, entityID string, limit int) ([]Event, error) {
	if limit <= 0 {
		limit = 100
	}

	const query = `SELECT ` + eventColumns + `
		FROM audit_events
		WHERE entity_type = $1 AND entity_id = $2
		ORDER BY occurred_at DESC, id DESC
		LIMIT $3`

	rows, err := q.Query(ctx, query, entityType, entityID, limit)
	if err != nil {
		return nil, fmt.Errorf("reading audit timeline: %w", err)
	}
	defer rows.Close()

	events := make([]Event, 0, limit)
	for rows.Next() {
		var (
			e      Event
			detail []byte
		)
		if err := rows.Scan(&e.Actor, &e.Action, &e.EntityType, &e.EntityID,
			&e.BeforeHash, &e.AfterHash, &e.TraceID, &detail, &e.OccurredAt); err != nil {
			return nil, fmt.Errorf("scanning audit event: %w", err)
		}
		if len(detail) > 0 {
			if err := json.Unmarshal(detail, &e.Detail); err != nil {
				return nil, fmt.Errorf("decoding audit detail: %w", err)
			}
		}
		e.OccurredAt = e.OccurredAt.UTC()
		events = append(events, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading audit timeline: %w", err)
	}
	return events, nil
}

// Of builds an event for the current request, filling in the trace id the caller arrived
// with so a timeline entry can be lined up with the log lines and the outbox delivery of
// the same request.
func Of(ctx context.Context, actor, action, entityType, entityID string, now time.Time) Event {
	return Event{
		Actor:      actor,
		Action:     action,
		EntityType: entityType,
		EntityID:   entityID,
		TraceID:    httpserver.TraceIDFrom(ctx),
		Detail:     map[string]any{},
		OccurredAt: now.UTC(),
	}
}

// With attaches a detail field and returns the event, so a call site reads as one
// expression.
//
// The detail map is copied rather than shared: an event built from another must not be
// able to change what that one records, or two entries derived from the same base would
// silently report the same details.
func (e Event) With(key string, value any) Event {
	detail := make(map[string]any, len(e.Detail)+1)
	for k, v := range e.Detail {
		detail[k] = v
	}
	detail[key] = value

	e.Detail = detail
	return e
}

// Between records the state on either side of the change.
func (e Event) Between(before, after any) Event {
	e.BeforeHash = Hash(before)
	e.AfterHash = Hash(after)
	return e
}
