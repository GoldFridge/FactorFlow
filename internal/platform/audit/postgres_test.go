package audit_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoldFridge/factorflow/internal/platform/audit"
	"github.com/GoldFridge/factorflow/internal/platform/pgtest"
	"github.com/GoldFridge/factorflow/internal/platform/postgres"
)

func TestMain(m *testing.M) {
	code := m.Run()
	pgtest.Shutdown()
	os.Exit(code)
}

func event(action, entityID string, at time.Time) audit.Event {
	return audit.Event{
		Actor:      "11111111-1111-4111-8111-111111111111",
		Action:     action,
		EntityType: "invoice",
		EntityID:   entityID,
		BeforeHash: audit.Hash(map[string]any{"status": "UPLOADED"}),
		AfterHash:  audit.Hash(map[string]any{"status": "APPROVED"}),
		TraceID:    "trace-4711",
		Detail:     map[string]any{"status": "APPROVED", "attempt": float64(1)},
		OccurredAt: at,
	}
}

func TestRecordAndReadBack(t *testing.T) {
	db := pgtest.New(t)
	ctx := context.Background()
	recorder := audit.NewPostgresRecorder()

	written := event("invoice.approved", "7a2f", testNow)
	require.NoError(t, recorder.Record(ctx, db.Querier(), written))

	events, err := recorder.Timeline(ctx, db.Querier(), "invoice", "7a2f", 0)
	require.NoError(t, err)
	require.Len(t, events, 1)

	got := events[0]
	assert.Equal(t, written.Actor, got.Actor)
	assert.Equal(t, written.Action, got.Action)
	assert.Equal(t, written.BeforeHash, got.BeforeHash)
	assert.Equal(t, written.AfterHash, got.AfterHash)
	assert.Equal(t, written.TraceID, got.TraceID)
	assert.Equal(t, written.Detail, got.Detail, "the detail survives the JSONB round trip")
	assert.True(t, got.OccurredAt.Equal(testNow))

	empty, err := recorder.Timeline(ctx, db.Querier(), "invoice", "unknown", 0)
	require.NoError(t, err)
	assert.Empty(t, empty, "an entity with no history is not an error")
}

func TestTimelineIsNewestFirstAndBounded(t *testing.T) {
	db := pgtest.New(t)
	ctx := context.Background()
	recorder := audit.NewPostgresRecorder()

	actions := []string{"invoice.created", "invoice.document_attached", "invoice.approved"}
	for i, action := range actions {
		require.NoError(t, recorder.Record(ctx, db.Querier(),
			event(action, "7a2f", testNow.Add(time.Duration(i)*time.Minute))))
	}
	// Another entity's history must not appear in this one.
	require.NoError(t, recorder.Record(ctx, db.Querier(), event("invoice.created", "b19c", testNow)))

	events, err := recorder.Timeline(ctx, db.Querier(), "invoice", "7a2f", 0)
	require.NoError(t, err)
	require.Len(t, events, 3)
	assert.Equal(t, "invoice.approved", events[0].Action, "the most recent entry comes first")
	assert.Equal(t, "invoice.created", events[2].Action)

	limited, err := recorder.Timeline(ctx, db.Querier(), "invoice", "7a2f", 2)
	require.NoError(t, err)
	assert.Len(t, limited, 2)
}

// TestAnAuditRowCommitsWithItsChange is the property the whole package exists for: the
// entry cannot survive a transaction that rolled back, so the timeline can never claim
// something happened that did not.
func TestAnAuditRowCommitsWithItsChange(t *testing.T) {
	db := pgtest.New(t)
	ctx := context.Background()
	recorder := audit.NewPostgresRecorder()

	failure := assert.AnError
	err := db.InTx(ctx, func(q postgres.Querier) error {
		if err := recorder.Record(ctx, q, event("invoice.approved", "7a2f", testNow)); err != nil {
			return err
		}
		return failure
	})
	require.ErrorIs(t, err, failure)

	events, err := recorder.Timeline(ctx, db.Querier(), "invoice", "7a2f", 0)
	require.NoError(t, err)
	assert.Empty(t, events)
}

func TestRecordRejectsAnEventWithNoSubject(t *testing.T) {
	db := pgtest.New(t)

	err := audit.NewPostgresRecorder().Record(context.Background(), db.Querier(), audit.Event{})
	require.Error(t, err)
}
