package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/GoldFridge/factorflow/migrations"
)

// dialect is fixed: the schema uses PostgreSQL features (partial unique indexes, JSONB,
// arrays) and is not portable by design.
const dialect = "postgres"

// Migrate applies every pending migration.
//
// goose speaks database/sql, so the pool is adapted rather than a second connection opened:
// one set of credentials, one place that knows how to reach the database.
func Migrate(ctx context.Context, db *DB) error {
	goose.SetBaseFS(migrations.FS)
	if err := goose.SetDialect(dialect); err != nil {
		return fmt.Errorf("setting goose dialect: %w", err)
	}

	sqlDB := stdlib.OpenDBFromPool(db.Pool())
	defer func() { _ = sqlDB.Close() }()

	if err := goose.UpContext(ctx, sqlDB, "."); err != nil {
		return fmt.Errorf("applying migrations: %w", err)
	}
	return nil
}

// MigrateDown rolls back the most recent migration. It exists for local development and
// for the migration tests; a deploy rolls forward.
func MigrateDown(ctx context.Context, db *DB) error {
	goose.SetBaseFS(migrations.FS)
	if err := goose.SetDialect(dialect); err != nil {
		return fmt.Errorf("setting goose dialect: %w", err)
	}

	sqlDB := stdlib.OpenDBFromPool(db.Pool())
	defer func() { _ = sqlDB.Close() }()

	if err := goose.DownContext(ctx, sqlDB, "."); err != nil {
		return fmt.Errorf("rolling back migration: %w", err)
	}
	return nil
}

// MigrateReset rolls every migration back, newest first.
//
// It exists for the migration test rather than for a deploy: a down migration nobody ever
// runs is a rollback that fails the first time it is needed, and only the oldest one is
// exercised by rolling back a single step.
func MigrateReset(ctx context.Context, db *DB) error {
	goose.SetBaseFS(migrations.FS)
	if err := goose.SetDialect(dialect); err != nil {
		return fmt.Errorf("setting goose dialect: %w", err)
	}

	sqlDB := stdlib.OpenDBFromPool(db.Pool())
	defer func() { _ = sqlDB.Close() }()

	if err := goose.DownToContext(ctx, sqlDB, ".", 0); err != nil {
		return fmt.Errorf("rolling back every migration: %w", err)
	}
	return nil
}

// MigrationVersion reports the applied schema version, which the readiness endpoint and
// the audit trail both report.
func MigrationVersion(ctx context.Context, db *DB) (int64, error) {
	goose.SetBaseFS(migrations.FS)
	if err := goose.SetDialect(dialect); err != nil {
		return 0, fmt.Errorf("setting goose dialect: %w", err)
	}

	sqlDB := stdlib.OpenDBFromPool(db.Pool())
	defer func() { _ = sqlDB.Close() }()

	version, err := goose.GetDBVersionContext(ctx, sqlDB)
	if err != nil {
		return 0, fmt.Errorf("reading schema version: %w", err)
	}
	return version, nil
}
