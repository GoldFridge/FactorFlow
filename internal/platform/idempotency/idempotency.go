// Package idempotency makes a repeated write safe to send.
//
// Every write endpoint carries an Idempotency-Key. A client that never sees a response,
// because its connection dropped or a proxy timed out, retries with the same key and gets
// the first response back instead of performing the effect twice. That matters most where
// the effect is a chain transaction or a payment, which cannot be undone by a retry.
//
// The key is scoped to an organization and an endpoint, and bound to a fingerprint of the
// request body: reusing a key for a different body is a client bug, and it is reported
// rather than silently served the earlier answer.
package idempotency

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/skimer2king/factorflow/internal/platform/apperr"
	"github.com/skimer2king/factorflow/internal/platform/postgres"
)

// MaxKeyLen bounds the client-supplied key.
const MaxKeyLen = 200

// Record is one reserved or completed idempotent request.
type Record struct {
	Key            string
	OrganizationID uuid.UUID
	Endpoint       string
	RequestHash    string
	StatusCode     int
	ResponseBody   json.RawMessage
	CreatedAt      time.Time
	CompletedAt    *time.Time
}

// IsComplete reports whether the first attempt finished and its response was stored.
func (r *Record) IsComplete() bool { return r.CompletedAt != nil }

// Fingerprint hashes the request body so a key reused with different content is detected.
func Fingerprint(method, path string, body []byte) string {
	digest := sha256.New()
	digest.Write([]byte(method))
	digest.Write([]byte{0})
	digest.Write([]byte(path))
	digest.Write([]byte{0})
	digest.Write(body)
	return hex.EncodeToString(digest.Sum(nil))
}

// Store persists idempotency records.
type Store struct{}

// NewStore returns the store.
func NewStore() *Store { return &Store{} }

// Reserve claims a key for a request.
//
// It returns nil when the caller owns the claim and should run the handler. It returns the
// existing record when the key was seen before, which the caller replays or rejects.
func (s *Store) Reserve(ctx context.Context, q postgres.Querier, rec *Record) (*Record, error) {
	const query = `
		INSERT INTO idempotency_keys (key, organization_id, endpoint, request_hash, created_at)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (organization_id, endpoint, key) DO NOTHING`

	tag, err := q.Exec(ctx, query, rec.Key, rec.OrganizationID, rec.Endpoint, rec.RequestHash, rec.CreatedAt.UTC())
	if err != nil {
		return nil, postgres.Translate(err)
	}
	if tag.RowsAffected() == 1 {
		return nil, nil
	}
	return s.Get(ctx, q, rec.OrganizationID, rec.Endpoint, rec.Key)
}

// Get returns a stored record.
func (s *Store) Get(ctx context.Context, q postgres.Querier, orgID uuid.UUID, endpoint, key string) (*Record, error) {
	const query = `
		SELECT key, organization_id, endpoint, request_hash, status_code, response_body, created_at, completed_at
		  FROM idempotency_keys
		 WHERE organization_id = $1 AND endpoint = $2 AND key = $3`

	var (
		rec         Record
		statusCode  *int
		body        []byte
		createdAt   time.Time
		completedAt *time.Time
	)
	err := q.QueryRow(ctx, query, orgID, endpoint, key).Scan(
		&rec.Key, &rec.OrganizationID, &rec.Endpoint, &rec.RequestHash, &statusCode, &body, &createdAt, &completedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, apperr.NotFoundf("idempotency key %s", key)
		}
		return nil, postgres.Translate(err)
	}

	if statusCode != nil {
		rec.StatusCode = *statusCode
	}
	rec.ResponseBody = body
	rec.CreatedAt = createdAt.UTC()
	if completedAt != nil {
		utc := completedAt.UTC()
		rec.CompletedAt = &utc
	}
	return &rec, nil
}

// Complete stores the response of a finished request, so a retry replays it.
func (s *Store) Complete(ctx context.Context, q postgres.Querier, rec *Record, status int, body []byte, now time.Time) error {
	const query = `
		UPDATE idempotency_keys
		   SET status_code = $4, response_body = $5, completed_at = $6
		 WHERE organization_id = $1 AND endpoint = $2 AND key = $3 AND completed_at IS NULL`

	payload := body
	if len(payload) == 0 {
		payload = []byte("null")
	}
	if !json.Valid(payload) {
		// Only JSON responses are replayable; anything else is stored as absent so the
		// retry re-runs rather than replaying a body the column cannot hold.
		payload = []byte("null")
	}

	_, err := q.Exec(ctx, query, rec.OrganizationID, rec.Endpoint, rec.Key, status, payload, now.UTC())
	return postgres.Translate(err)
}

// Release removes a claim whose request failed, so the client may retry it.
//
// Only failures are released. A successful response stays recorded: that is the whole
// point, and re-running a successful chain call would be exactly the double effect this
// package exists to prevent.
func (s *Store) Release(ctx context.Context, q postgres.Querier, rec *Record) error {
	const query = `
		DELETE FROM idempotency_keys
		 WHERE organization_id = $1 AND endpoint = $2 AND key = $3 AND completed_at IS NULL`

	_, err := q.Exec(ctx, query, rec.OrganizationID, rec.Endpoint, rec.Key)
	return postgres.Translate(err)
}

// Validate checks a client-supplied key.
func Validate(key string) error {
	switch {
	case key == "":
		return apperr.Invalid("Idempotency-Key", "header is required on write requests")
	case len(key) > MaxKeyLen:
		return apperr.Invalid("Idempotency-Key", "must be at most %d characters", MaxKeyLen)
	}
	for _, r := range key {
		if r < '!' || r > '~' {
			return apperr.Invalid("Idempotency-Key", "must contain only printable ASCII characters")
		}
	}
	return nil
}

// Errors this package reports to a client.
var (
	// ErrKeyReused reports a key reused with a different request body.
	ErrKeyReused = errors.New("the idempotency key was already used with a different request")
	// ErrInFlight reports a retry that arrived while the first attempt is still running.
	ErrInFlight = errors.New("a request with this idempotency key is still in progress")
)

// ConflictKeyReused wraps ErrKeyReused as a conflict for the transport layer.
func ConflictKeyReused() error {
	return apperr.Conflictf("%s", ErrKeyReused.Error())
}

// ConflictInFlight wraps ErrInFlight as a conflict for the transport layer.
func ConflictInFlight() error {
	return apperr.Conflictf("%s", ErrInFlight.Error())
}
