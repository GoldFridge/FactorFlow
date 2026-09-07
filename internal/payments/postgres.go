package payments

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/GoldFridge/factorflow/internal/platform/apperr"
	"github.com/GoldFridge/factorflow/internal/platform/money"
	"github.com/GoldFridge/factorflow/internal/platform/postgres"
)

// Repository stores paid requests.
type Repository interface {
	Create(ctx context.Context, q postgres.Querier, r *Request) error
	Get(ctx context.Context, q postgres.Querier, id uuid.UUID) (*Request, error)
	GetByNonce(ctx context.Context, q postgres.Querier, nonce string) (*Request, error)
	GetByIdempotencyKey(ctx context.Context, q postgres.Querier, endpoint, key string) (*Request, error)
	Update(ctx context.Context, q postgres.Querier, r *Request, expectedVersion int64) error
	DeleteExpired(ctx context.Context, q postgres.Querier, before time.Time) (int64, error)
}

// PostgresRepository is the PostgreSQL implementation of Repository.
type PostgresRepository struct{}

// NewPostgresRepository returns the repository.
func NewPostgresRepository() *PostgresRepository { return &PostgresRepository{} }

const requestColumns = `
	id, endpoint, request_hash, nonce, idempotency_key, price_minor, currency, payer,
	payment_tx, response_hash, response, state, reason, version, created_at, updated_at,
	expires_at`

// Create inserts a quoted price.
func (r *PostgresRepository) Create(ctx context.Context, q postgres.Querier, req *Request) error {
	const query = `
		INSERT INTO x402_requests (` + requestColumns + `)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)`

	_, err := q.Exec(ctx, query,
		req.ID, req.Endpoint, req.RequestHash, req.Nonce, req.IdempotencyKey,
		req.Price.Minor(), req.Price.Currency().String(), req.Payer, req.PaymentTx,
		req.ResponseHash, req.Response, req.State.String(), req.Reason, req.Version,
		req.CreatedAt, req.UpdatedAt, req.ExpiresAt)
	return postgres.Translate(err)
}

// Get returns one paid request.
func (r *PostgresRepository) Get(ctx context.Context, q postgres.Querier, id uuid.UUID) (*Request, error) {
	const query = `SELECT ` + requestColumns + ` FROM x402_requests WHERE id = $1`

	req, err := scanRequest(q.QueryRow(ctx, query, id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, apperr.NotFoundf("paid request %s", id)
		}
		return nil, postgres.Translate(err)
	}
	return req, nil
}

// GetByNonce finds the quote a payment claims to be for.
func (r *PostgresRepository) GetByNonce(ctx context.Context, q postgres.Querier, nonce string) (*Request, error) {
	const query = `SELECT ` + requestColumns + ` FROM x402_requests WHERE nonce = $1`

	req, err := scanRequest(q.QueryRow(ctx, query, nonce))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, apperr.NotFoundf("paid request for nonce %s", nonce)
		}
		return nil, postgres.Translate(err)
	}
	return req, nil
}

// GetByIdempotencyKey finds a client's earlier request under the same key.
func (r *PostgresRepository) GetByIdempotencyKey(ctx context.Context, q postgres.Querier, endpoint, key string) (*Request, error) {
	const query = `
		SELECT ` + requestColumns + `
		  FROM x402_requests
		 WHERE endpoint = $1 AND idempotency_key = $2`

	req, err := scanRequest(q.QueryRow(ctx, query, endpoint, key))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, apperr.NotFoundf("paid request for key %s", key)
		}
		return nil, postgres.Translate(err)
	}
	return req, nil
}

// Update stores a request under the version it was read at.
func (r *PostgresRepository) Update(ctx context.Context, q postgres.Querier, req *Request, expectedVersion int64) error {
	const query = `
		UPDATE x402_requests
		   SET payer = $3, payment_tx = $4, response_hash = $5, response = $6,
		       state = $7, reason = $8, version = $9, updated_at = $10
		 WHERE id = $1 AND version = $2`

	tag, err := q.Exec(ctx, query,
		req.ID, expectedVersion, req.Payer, req.PaymentTx, req.ResponseHash, req.Response,
		req.State.String(), req.Reason, req.Version, req.UpdatedAt)
	if err != nil {
		return postgres.Translate(err)
	}
	if tag.RowsAffected() == 0 {
		return apperr.Conflictf("paid request %s was modified concurrently", req.ID)
	}
	return nil
}

// DeleteExpired removes quotes nobody paid.
//
// Only unpaid quotes are removed. A paid exchange is kept: it is the record of what a
// client bought, and the client may still ask for its answer again.
func (r *PostgresRepository) DeleteExpired(ctx context.Context, q postgres.Querier, before time.Time) (int64, error) {
	const query = `DELETE FROM x402_requests WHERE state = 'PAYMENT_REQUIRED' AND expires_at < $1`

	tag, err := q.Exec(ctx, query, before)
	if err != nil {
		return 0, postgres.Translate(err)
	}
	return tag.RowsAffected(), nil
}

// row is the shared shape of pgx.Row and pgx.Rows for scanning.
type row interface {
	Scan(dest ...any) error
}

func scanRequest(r row) (*Request, error) {
	var (
		req        Request
		priceMinor int64
		currency   string
		state      string
		createdAt  time.Time
		updatedAt  time.Time
		expiresAt  time.Time
	)

	if err := r.Scan(&req.ID, &req.Endpoint, &req.RequestHash, &req.Nonce, &req.IdempotencyKey,
		&priceMinor, &currency, &req.Payer, &req.PaymentTx, &req.ResponseHash, &req.Response,
		&state, &req.Reason, &req.Version, &createdAt, &updatedAt, &expiresAt); err != nil {
		return nil, err
	}

	parsedCurrency, err := money.ParseCurrency(currency)
	if err != nil {
		return nil, err
	}
	if req.Price, err = money.New(priceMinor, parsedCurrency); err != nil {
		return nil, err
	}
	if req.State, err = ParseState(state); err != nil {
		return nil, err
	}

	req.CreatedAt = createdAt.UTC()
	req.UpdatedAt = updatedAt.UTC()
	req.ExpiresAt = expiresAt.UTC()
	return &req, nil
}
