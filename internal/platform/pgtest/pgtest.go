// Package pgtest starts a real PostgreSQL for integration tests.
//
// The repositories are tested against the database they actually run on, not a mock: half
// of what a repository does is expressed in SQL, constraints and transactions, and a mock
// would only ever confirm that the code calls itself.
//
// One container is shared by every test in a package. Each test gets its own schema-clean
// database state through Truncate, which is far faster than a container per test and still
// leaves tests independent of each other's rows.
package pgtest

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/GoldFridge/factorflow/internal/platform/postgres"
)

// image pins the database version so a test failure is never the test environment drifting.
const image = "postgres:17-alpine"

// EnvDatabaseURL points the tests at an already-running database instead of starting a
// container. CI sets it to a service container; a developer can set it to a local instance.
const EnvDatabaseURL = "FF_TEST_DATABASE_URL"

var (
	once      sync.Once
	shared    *postgres.DB
	sharedErr error
	cleanup   func()
)

// New returns a migrated database for the test, skipping the test when no database is
// available.
//
// Skipping rather than failing is deliberate: unit tests must run on a laptop with no
// Docker daemon, while `make test-all` and CI, which do have one, still run everything.
func New(t *testing.T) *postgres.DB {
	t.Helper()

	if testing.Short() {
		t.Skip("integration test: needs PostgreSQL, skipped in short mode")
	}

	once.Do(func() { shared, cleanup, sharedErr = start() })
	if sharedErr != nil {
		t.Skipf("integration test: no PostgreSQL available (%v)", sharedErr)
	}

	Truncate(t, shared)
	return shared
}

// start brings up the database, preferring an external one when the environment names it.
func start() (*postgres.DB, func(), error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	if url := os.Getenv(EnvDatabaseURL); url != "" {
		db, err := postgres.Connect(ctx, postgres.DefaultConfig(url))
		if err != nil {
			return nil, nil, err
		}
		if err := postgres.Migrate(ctx, db); err != nil {
			db.Close()
			return nil, nil, err
		}
		return db, db.Close, nil
	}

	container, err := tcpostgres.Run(ctx, image,
		tcpostgres.WithDatabase("factorflow"),
		tcpostgres.WithUsername("factorflow"),
		tcpostgres.WithPassword("factorflow"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(2*time.Minute),
		),
	)
	if err != nil {
		return nil, nil, err
	}

	url, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		_ = testcontainers.TerminateContainer(container)
		return nil, nil, err
	}

	db, err := postgres.Connect(ctx, postgres.DefaultConfig(url))
	if err != nil {
		_ = testcontainers.TerminateContainer(container)
		return nil, nil, err
	}
	if err := postgres.Migrate(ctx, db); err != nil {
		db.Close()
		_ = testcontainers.TerminateContainer(container)
		return nil, nil, err
	}

	return db, func() {
		db.Close()
		_ = testcontainers.TerminateContainer(container)
	}, nil
}

// tables are truncated between tests, ordered so the statement is one command and cascades
// handle the foreign keys.
const truncateStatement = `
TRUNCATE TABLE
    idempotency_keys,
    outbox_events,
    audit_events,
    allocation_certificates,
    allocation_rejections,
    allocations,
    bids,
    auction_lots,
    auctions,
    tokenized_assets,
    risk_assessments,
    market_snapshots,
    invoice_documents,
    invoices,
    memberships,
    sessions,
    auth_challenges,
    organizations
RESTART IDENTITY CASCADE`

// Truncate empties every table, leaving the schema in place.
func Truncate(t *testing.T, db *postgres.DB) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := db.Querier().Exec(ctx, truncateStatement); err != nil {
		t.Fatalf("truncating test database: %v", err)
	}
}

// Shutdown stops the shared container. A package with integration tests calls it from
// TestMain so the container does not outlive the test binary.
func Shutdown() {
	if cleanup != nil {
		cleanup()
	}
}
