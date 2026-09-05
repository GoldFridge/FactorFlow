package invoice_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/skimer2king/factorflow/internal/invoice"
	"github.com/skimer2king/factorflow/internal/platform/apperr"
)

func TestParseStatus(t *testing.T) {
	t.Parallel()

	got, err := invoice.ParseStatus("AUCTION_OPEN")
	require.NoError(t, err)
	assert.Equal(t, invoice.StatusAuctionOpen, got)
	assert.Equal(t, "AUCTION_OPEN", got.String())

	_, err = invoice.ParseStatus("auction_open")
	require.ErrorIs(t, err, apperr.ErrValidation, "the wire format is case sensitive")

	_, err = invoice.ParseStatus("PAID")
	require.ErrorIs(t, err, apperr.ErrValidation)
}

func TestTerminalStates(t *testing.T) {
	t.Parallel()

	terminal := map[invoice.Status]bool{
		invoice.StatusRejected:  true,
		invoice.StatusMatured:   true,
		invoice.StatusDefaulted: true,
	}

	for _, status := range invoice.AllStatuses() {
		assert.Equalf(t, terminal[status], status.IsTerminal(), "IsTerminal(%s)", status)
	}

	assert.False(t, invoice.Status("PAID").IsTerminal(), "an unknown status is not terminal")
}

// TestTransitionTableIsClosed proves the state machine cannot point at a state that does
// not exist, which is what would let a stored invoice become unmovable.
func TestTransitionTableIsClosed(t *testing.T) {
	t.Parallel()

	for _, from := range invoice.AllStatuses() {
		require.Truef(t, from.IsValid(), "source status %s is not registered", from)

		for _, to := range invoice.TransitionsFrom(from) {
			assert.Truef(t, to.IsValid(), "%s points at unknown status %s", from, to)
			assert.NotEqualf(t, from, to, "%s has a self transition", from)
		}
	}
}

// TestEveryStateIsReachable proves no state is dead code: each one except the initial
// DRAFT is the target of at least one transition.
func TestEveryStateIsReachable(t *testing.T) {
	t.Parallel()

	reachable := map[invoice.Status]bool{invoice.StatusDraft: true}
	for _, from := range invoice.AllStatuses() {
		for _, to := range invoice.TransitionsFrom(from) {
			reachable[to] = true
		}
	}

	for _, status := range invoice.AllStatuses() {
		assert.Truef(t, reachable[status], "status %s is unreachable", status)
	}
}

func TestCanTransitionTo(t *testing.T) {
	t.Parallel()

	assert.True(t, invoice.StatusDraft.CanTransitionTo(invoice.StatusUploaded))
	assert.True(t, invoice.StatusDraft.CanTransitionTo(invoice.StatusRejected))
	assert.False(t, invoice.StatusDraft.CanTransitionTo(invoice.StatusApproved))
	assert.False(t, invoice.StatusMatured.CanTransitionTo(invoice.StatusSettled))
	assert.False(t, invoice.Status("PAID").CanTransitionTo(invoice.StatusDraft))
}
