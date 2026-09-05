// Package postgres holds the database plumbing every module's repository builds on: a
// pool, a transaction runner, and the migration runner.
//
// It contains no business rules. A repository in a domain module owns its SQL; this
// package owns connections, transactions and error translation, so those concerns are
// solved once rather than in ten repositories.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/skimer2king/factorflow/internal/platform/apperr"
)

// PostgreSQL error codes the application reacts to.
const (
	codeUniqueViolation      = "23505"
	codeForeignKeyViolation  = "23503"
	codeCheckViolation       = "23514"
	codeSerializationFailure = "40001"
	codeDeadlockDetected     = "40P01"
)

// Config describes how to reach the database.
type Config struct {
	// URL is a libpq connection string.
	URL string
	// MaxConns bounds the pool. The demo runs on a 2 vCPU VPS, so a large pool would queue
	// inside PostgreSQL instead of inside the application, where the queue is visible.
	MaxConns int32
	// MaxConnLifetime recycles connections so a long-lived process does not hold a
	// connection through a database restart.
	MaxConnLifetime time.Duration
	// ConnectTimeout bounds the initial connection attempt.
	ConnectTimeout time.Duration
}

// DefaultConfig returns the settings the single-server deployment uses.
func DefaultConfig(url string) Config {
	return Config{
		URL:             url,
		MaxConns:        10,
		MaxConnLifetime: time.Hour,
		ConnectTimeout:  10 * time.Second,
	}
}

// DB is a connection pool with the helpers repositories need.
type DB struct {
	pool *pgxpool.Pool
}

// Querier is the subset of pgx both a pool and a transaction satisfy.
//
// Repositories take a Querier rather than a pool, so the same method works inside a
// transaction and outside one. That is what lets a service compose several repository
// calls into one atomic change without every repository knowing about transactions.
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Connect opens the pool and verifies the database answers.
func Connect(ctx context.Context, cfg Config) (*DB, error) {
	poolConfig, err := pgxpool.ParseConfig(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("parsing database url: %w", err)
	}
	if cfg.MaxConns > 0 {
		poolConfig.MaxConns = cfg.MaxConns
	}
	if cfg.MaxConnLifetime > 0 {
		poolConfig.MaxConnLifetime = cfg.MaxConnLifetime
	}

	connectCtx := ctx
	if cfg.ConnectTimeout > 0 {
		var cancel context.CancelFunc
		connectCtx, cancel = context.WithTimeout(ctx, cfg.ConnectTimeout)
		defer cancel()
	}

	pool, err := pgxpool.NewWithConfig(connectCtx, poolConfig)
	if err != nil {
		return nil, fmt.Errorf("connecting to postgres: %w", err)
	}
	if err := pool.Ping(connectCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("pinging postgres: %w", err)
	}

	return &DB{pool: pool}, nil
}

// Pool exposes the underlying pool for the few callers that need it, such as the
// migration runner.
func (db *DB) Pool() *pgxpool.Pool { return db.pool }

// Querier returns the pool as a Querier for work outside a transaction.
func (db *DB) Querier() Querier { return db.pool }

// Ping reports whether the database is reachable, backing the readiness probe.
func (db *DB) Ping(ctx context.Context) error {
	if err := db.pool.Ping(ctx); err != nil {
		return apperr.Unavailablef("postgres: %v", err)
	}
	return nil
}

// Close releases the pool.
func (db *DB) Close() { db.pool.Close() }

// InTx runs fn inside a transaction, committing when it returns nil and rolling back on
// any error or panic.
//
// The rule this enforces is the one the specification insists on: domain state and its
// outbox row are written together, and no chain call happens inside the transaction.
func (db *DB) InTx(ctx context.Context, fn func(Querier) error) error {
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return Translate(err)
	}

	committed := false
	defer func() {
		if !committed {
			// The rollback runs on its own context: the caller's may already be cancelled,
			// and an abandoned transaction holds locks until the connection is recycled.
			rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			_ = tx.Rollback(rollbackCtx)
		}
	}()

	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return Translate(err)
	}
	committed = true
	return nil
}

// Translate converts a driver error into the application's error kinds, so callers can
// react to "already exists" or "not found" without matching on driver types.
func Translate(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return apperr.NotFoundf("row does not exist")
	}

	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case codeUniqueViolation:
			return apperr.Conflictf("%s already exists", constraintSubject(pgErr))
		case codeForeignKeyViolation:
			return apperr.Conflictf("%s references a row that does not exist", constraintSubject(pgErr))
		case codeCheckViolation:
			return apperr.Invalid(constraintSubject(pgErr), "violates database constraint %s", pgErr.ConstraintName)
		case codeSerializationFailure, codeDeadlockDetected:
			// Concurrent writers, not a broken request: the caller may retry.
			return apperr.Conflictf("concurrent update, retry the request")
		}
	}
	return err
}

// constraintSubject names what a constraint was protecting, falling back to the table.
func constraintSubject(pgErr *pgconn.PgError) string {
	if pgErr.ConstraintName != "" {
		return pgErr.ConstraintName
	}
	if pgErr.TableName != "" {
		return pgErr.TableName
	}
	return "row"
}

// IsNotFound reports whether err came from a query that matched no rows.
func IsNotFound(err error) bool {
	return errors.Is(err, pgx.ErrNoRows) || errors.Is(err, apperr.ErrNotFound)
}
