package postgres_test

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/skimer2king/factorflow/internal/platform/apperr"
	"github.com/skimer2king/factorflow/internal/platform/pgtest"
	"github.com/skimer2king/factorflow/internal/platform/postgres"
)

func TestMain(m *testing.M) {
	code := m.Run()
	pgtest.Shutdown()
	os.Exit(code)
}

// TestMigrationsApplied checks that pgtest's database is the schema the migrations
// describe, which is what every repository test then relies on.
func TestMigrationsApplied(t *testing.T) {
	db := pgtest.New(t)
	ctx := context.Background()

	version, err := postgres.MigrationVersion(ctx, db)
	require.NoError(t, err)
	assert.Positive(t, version, "the schema is migrated, not created by hand")

	tables := []string{
		"organizations", "memberships", "invoices", "invoice_documents",
		"market_snapshots", "risk_assessments", "auctions", "auction_lots", "bids",
		"allocations", "allocation_rejections", "allocation_certificates",
		"audit_events", "outbox_events", "idempotency_keys",
	}
	for _, table := range tables {
		var exists bool
		err := db.Querier().QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = 'public' AND table_name = $1)`,
			table).Scan(&exists)
		require.NoError(t, err)
		assert.Truef(t, exists, "table %s is missing", table)
	}
}

// TestMigrationsAreReversible protects the rollback path a deploy depends on: a broken
// down-migration is only ever discovered when it is needed most.
func TestMigrationsAreReversible(t *testing.T) {
	db := pgtest.New(t)
	ctx := context.Background()

	require.NoError(t, postgres.MigrateDown(ctx, db))

	var exists bool
	require.NoError(t, db.Querier().QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = 'public' AND table_name = 'invoices')`).
		Scan(&exists))
	assert.False(t, exists, "the down migration removed the schema")

	require.NoError(t, postgres.Migrate(ctx, db), "and it can be applied again")
}

func TestPing(t *testing.T) {
	db := pgtest.New(t)
	require.NoError(t, db.Ping(context.Background()))
}

func TestConnectRejectsAnUnreachableDatabase(t *testing.T) {
	t.Parallel()

	_, err := postgres.Connect(context.Background(),
		postgres.DefaultConfig("postgres://nobody:nobody@127.0.0.1:1/factorflow?sslmode=disable"))
	require.Error(t, err)
}

func TestConnectRejectsAMalformedURL(t *testing.T) {
	t.Parallel()

	_, err := postgres.Connect(context.Background(), postgres.DefaultConfig("not a url"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parsing database url")
}

func TestTranslate(t *testing.T) {
	t.Parallel()

	assert.NoError(t, postgres.Translate(nil))

	// A driver error with no known code passes through unchanged, so an unexpected failure
	// is never mistaken for a client mistake.
	original := assert.AnError
	assert.ErrorIs(t, postgres.Translate(original), original)
}

// TestInTxRollsBackOnPanic checks the transaction helper's least-tested path: a panic must
// not leave a transaction open holding locks.
func TestInTxRollsBackOnPanic(t *testing.T) {
	db := pgtest.New(t)
	ctx := context.Background()

	assert.Panics(t, func() {
		_ = db.InTx(ctx, func(q postgres.Querier) error {
			_, err := q.Exec(ctx, `CREATE TABLE panic_probe (id INT)`)
			require.NoError(t, err)
			panic("boom")
		})
	})

	var exists bool
	require.NoError(t, db.Querier().QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = 'public' AND table_name = 'panic_probe')`).
		Scan(&exists))
	assert.False(t, exists, "the panicking transaction was rolled back")

	require.NoError(t, db.Ping(ctx), "and the pool is still usable")
}

func TestIsNotFound(t *testing.T) {
	t.Parallel()

	assert.True(t, postgres.IsNotFound(apperr.NotFoundf("invoice %s", "abc")))
	assert.False(t, postgres.IsNotFound(apperr.Conflictf("stale version")))
	assert.False(t, postgres.IsNotFound(nil))
}
