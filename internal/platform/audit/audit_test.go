package audit_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoldFridge/factorflow/internal/platform/apperr"
	"github.com/GoldFridge/factorflow/internal/platform/audit"
	"github.com/GoldFridge/factorflow/internal/platform/httpserver"
)

var testNow = time.Date(2026, time.September, 8, 9, 30, 0, 0, time.UTC)

func TestValidateRejectsAnEventWithNoSubject(t *testing.T) {
	t.Parallel()

	valid := audit.Event{
		Actor: "system", Action: "invoice.created",
		EntityType: "invoice", EntityID: "7a2f", OccurredAt: testNow,
	}
	require.NoError(t, valid.Validate())

	tests := []struct {
		name   string
		mutate func(*audit.Event)
	}{
		{name: "no actor", mutate: func(e *audit.Event) { e.Actor = "  " }},
		{name: "no action", mutate: func(e *audit.Event) { e.Action = "" }},
		{name: "no entity type", mutate: func(e *audit.Event) { e.EntityType = "" }},
		{name: "no entity id", mutate: func(e *audit.Event) { e.EntityID = "" }},
		{name: "no time", mutate: func(e *audit.Event) { e.OccurredAt = time.Time{} }},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := valid
			tc.mutate(&e)
			assert.ErrorIs(t, e.Validate(), apperr.ErrValidation)
		})
	}
}

// TestHashIsStableAndSensitive is what makes a before/after hash worth recording: a reader
// holding the state can recompute it, and a changed state does not hash the same.
func TestHashIsStableAndSensitive(t *testing.T) {
	t.Parallel()

	state := map[string]any{"status": "APPROVED", "version": 3}
	same := map[string]any{"version": 3, "status": "APPROVED"}

	assert.Equal(t, audit.Hash(state), audit.Hash(same), "key order is not part of the state")
	assert.NotEqual(t, audit.Hash(state), audit.Hash(map[string]any{"status": "APPROVED", "version": 4}))
	assert.Empty(t, audit.Hash(nil), "no prior state is no hash, not the hash of nothing")

	var absent map[string]any
	assert.Empty(t, audit.Hash(absent), "a typed nil is absence too, not a state")
	assert.Regexp(t, `^0x[0-9a-f]{64}$`, audit.Hash(state))

	// A value that cannot be encoded is a programming mistake, and must not panic a path
	// that is in the middle of committing a change.
	assert.Empty(t, audit.Hash(make(chan int)))
}

// TestOfCarriesTheTrace is what ties a timeline entry to the request that caused it.
func TestOfCarriesTheTrace(t *testing.T) {
	t.Parallel()

	ctx := httpserver.ContextWithTraceID(t.Context(), "trace-4711")
	e := audit.Of(ctx, "issuer-1", "invoice.approved", "invoice", "7a2f", testNow.Local())

	require.NoError(t, e.Validate())
	assert.Equal(t, "trace-4711", e.TraceID)
	assert.Equal(t, testNow, e.OccurredAt, "times are recorded in UTC")
	assert.Empty(t, audit.Of(t.Context(), "issuer-1", "a", "invoice", "7a2f", testNow).TraceID,
		"work with no request behind it simply has no trace")
}

func TestBuildersDoNotShareState(t *testing.T) {
	t.Parallel()

	base := audit.Of(t.Context(), "issuer-1", "invoice.approved", "invoice", "7a2f", testNow)

	first := base.With("reason", "looks good")
	second := base.With("reason", "different")

	assert.Equal(t, "looks good", first.Detail["reason"])
	assert.Equal(t, "different", second.Detail["reason"],
		"two events built from one base must not share their details")
	assert.Empty(t, base.Detail, "the base event is left alone")

	withState := base.Between(map[string]any{"status": "PENDING"}, map[string]any{"status": "APPROVED"})
	assert.NotEqual(t, withState.BeforeHash, withState.AfterHash)
	assert.Empty(t, base.BeforeHash, "the original event is left alone")
}

func TestDiscardRecordsNothing(t *testing.T) {
	t.Parallel()

	assert.NoError(t, audit.Discard{}.Record(t.Context(), nil, audit.Event{}),
		"a service wired without a trail must not fail its writes")
}
