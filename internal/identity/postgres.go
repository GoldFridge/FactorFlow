package identity

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/GoldFridge/factorflow/internal/platform/apperr"
	"github.com/GoldFridge/factorflow/internal/platform/postgres"
)

// Repository stores login challenges and sessions.
type Repository interface {
	SaveChallenge(ctx context.Context, q postgres.Querier, challenge *Challenge) error
	GetChallenge(ctx context.Context, q postgres.Querier, nonce string) (*Challenge, error)
	ConsumeChallenge(ctx context.Context, q postgres.Querier, challenge *Challenge) error
	DeleteExpiredChallenges(ctx context.Context, q postgres.Querier, before time.Time) (int64, error)

	SaveSession(ctx context.Context, q postgres.Querier, session *Session) error
	GetSession(ctx context.Context, q postgres.Querier, tokenHash string) (*Session, error)
	RevokeSession(ctx context.Context, q postgres.Querier, session *Session) error
}

// PostgresRepository is the PostgreSQL implementation of Repository.
type PostgresRepository struct{}

// NewPostgresRepository returns the repository.
func NewPostgresRepository() *PostgresRepository { return &PostgresRepository{} }

const challengeColumns = `nonce, wallet, issued_at, expires_at, consumed_at`

const sessionColumns = `token_hash, organization_id, wallet, issued_at, expires_at, revoked_at`

// SaveChallenge stores a freshly minted challenge.
func (r *PostgresRepository) SaveChallenge(ctx context.Context, q postgres.Querier, challenge *Challenge) error {
	const query = `
		INSERT INTO auth_challenges (` + challengeColumns + `)
		VALUES ($1, $2, $3, $4, $5)`

	_, err := q.Exec(ctx, query,
		challenge.Nonce, challenge.Wallet, challenge.IssuedAt, challenge.ExpiresAt, challenge.ConsumedAt)
	return postgres.Translate(err)
}

// GetChallenge returns one challenge by its nonce.
func (r *PostgresRepository) GetChallenge(ctx context.Context, q postgres.Querier, nonce string) (*Challenge, error) {
	const query = `SELECT ` + challengeColumns + ` FROM auth_challenges WHERE nonce = $1`

	var (
		challenge  Challenge
		issuedAt   time.Time
		expiresAt  time.Time
		consumedAt *time.Time
	)
	err := q.QueryRow(ctx, query, nonce).Scan(
		&challenge.Nonce, &challenge.Wallet, &issuedAt, &expiresAt, &consumedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, apperr.NotFoundf("login challenge")
		}
		return nil, postgres.Translate(err)
	}

	challenge.IssuedAt = issuedAt.UTC()
	challenge.ExpiresAt = expiresAt.UTC()
	if consumedAt != nil {
		utc := consumedAt.UTC()
		challenge.ConsumedAt = &utc
	}
	return &challenge, nil
}

// ConsumeChallenge marks a challenge used, and refuses if another request got there first.
//
// The conditional update is the guard: two requests racing with the same signature must not
// both mint a session, so the database decides which one wins.
func (r *PostgresRepository) ConsumeChallenge(ctx context.Context, q postgres.Querier, challenge *Challenge) error {
	const query = `
		UPDATE auth_challenges
		   SET consumed_at = $2
		 WHERE nonce = $1 AND consumed_at IS NULL`

	tag, err := q.Exec(ctx, query, challenge.Nonce, challenge.ConsumedAt)
	if err != nil {
		return postgres.Translate(err)
	}
	if tag.RowsAffected() == 0 {
		return apperr.Conflictf("challenge was already used")
	}
	return nil
}

// DeleteExpiredChallenges removes challenges nobody can use any more.
func (r *PostgresRepository) DeleteExpiredChallenges(ctx context.Context, q postgres.Querier, before time.Time) (int64, error) {
	const query = `DELETE FROM auth_challenges WHERE expires_at < $1`

	tag, err := q.Exec(ctx, query, before.UTC())
	if err != nil {
		return 0, postgres.Translate(err)
	}
	return tag.RowsAffected(), nil
}

// SaveSession stores a session.
func (r *PostgresRepository) SaveSession(ctx context.Context, q postgres.Querier, session *Session) error {
	const query = `
		INSERT INTO sessions (` + sessionColumns + `)
		VALUES ($1, $2, $3, $4, $5, $6)`

	_, err := q.Exec(ctx, query,
		session.TokenHash, session.OrganizationID, session.Wallet,
		session.IssuedAt, session.ExpiresAt, session.RevokedAt)
	return postgres.Translate(err)
}

// GetSession returns a session by the hash of its token.
func (r *PostgresRepository) GetSession(ctx context.Context, q postgres.Querier, tokenHash string) (*Session, error) {
	const query = `SELECT ` + sessionColumns + ` FROM sessions WHERE token_hash = $1`

	var (
		session   Session
		issuedAt  time.Time
		expiresAt time.Time
		revokedAt *time.Time
	)
	err := q.QueryRow(ctx, query, tokenHash).Scan(
		&session.TokenHash, &session.OrganizationID, &session.Wallet, &issuedAt, &expiresAt, &revokedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, apperr.NotFoundf("session")
		}
		return nil, postgres.Translate(err)
	}

	session.IssuedAt = issuedAt.UTC()
	session.ExpiresAt = expiresAt.UTC()
	if revokedAt != nil {
		utc := revokedAt.UTC()
		session.RevokedAt = &utc
	}
	return &session, nil
}

// RevokeSession ends a session.
func (r *PostgresRepository) RevokeSession(ctx context.Context, q postgres.Querier, session *Session) error {
	const query = `
		UPDATE sessions
		   SET revoked_at = $2
		 WHERE token_hash = $1 AND revoked_at IS NULL`

	_, err := q.Exec(ctx, query, session.TokenHash, session.RevokedAt)
	return postgres.Translate(err)
}
