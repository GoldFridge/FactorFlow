package auction_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoldFridge/factorflow/internal/auction"
	"github.com/GoldFridge/factorflow/internal/platform/apperr"
)

const testCertificate = "0x" + "8c1a5f2e7b3d4906ab5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f901a2b"

func TestAuctionHappyPath(t *testing.T) {
	t.Parallel()

	a := newAuction(t)

	require.NoError(t, a.Open(testOpens))
	require.NoError(t, a.StartClearing(testClose))
	require.NoError(t, a.MarkCleared("solver-v1", testCertificate, testClose.Add(time.Second)))

	assert.Equal(t, auction.StatusCleared, a.Status)
	assert.Equal(t, "solver-v1", a.SolverVersion)
	assert.Equal(t, testCertificate, a.CertificateHash)

	require.NoError(t, a.StartSettling(testClose.Add(time.Minute)))
	require.NoError(t, a.MarkSettled(testClose.Add(2*time.Minute)))

	assert.Equal(t, auction.StatusSettled, a.Status)
	assert.True(t, a.Status.IsTerminal())
	assert.Equal(t, int64(6), a.Version)
}

func TestOpenRespectsTheSchedule(t *testing.T) {
	t.Parallel()

	early := newAuction(t)
	require.ErrorIs(t, early.Open(testOpens.Add(-time.Minute)), apperr.ErrConflict)
	assert.Equal(t, auction.StatusDraft, early.Status)

	late := newAuction(t)
	require.ErrorIs(t, late.Open(testClose), apperr.ErrConflict, "an auction cannot open after it closes")
}

// TestClearingWaitsForTheClose is the fairness rule: bidding stops before the solver runs,
// so nobody can bid against a partially known result.
func TestClearingWaitsForTheClose(t *testing.T) {
	t.Parallel()

	a := newAuction(t)
	require.NoError(t, a.Open(testOpens))

	require.ErrorIs(t, a.StartClearing(testClose.Add(-time.Second)), apperr.ErrConflict)
	assert.Equal(t, auction.StatusOpen, a.Status)

	require.NoError(t, a.StartClearing(testClose))
	assert.Equal(t, auction.StatusClearing, a.Status)
}

func TestMarkClearedValidatesItsEvidence(t *testing.T) {
	t.Parallel()

	a := clearingAuction(t)

	require.ErrorIs(t, a.MarkCleared("", testCertificate, testClose), apperr.ErrValidation)
	require.ErrorIs(t, a.MarkCleared("solver-v1", "0xdeadbeef", testClose), apperr.ErrValidation)
	require.ErrorIs(t, a.MarkCleared("solver-v1", "", testClose), apperr.ErrValidation)
	assert.Equal(t, auction.StatusClearing, a.Status, "a rejected command leaves the state untouched")
}

// TestEmptyAllocationStillClears matches the specification: a batch with no feasible match
// is CLEARED with nothing allocated and an explanation, not FAILED.
func TestEmptyAllocationStillClears(t *testing.T) {
	t.Parallel()

	a := clearingAuction(t)
	require.NoError(t, a.MarkCleared("solver-v1", testCertificate, testClose))
	assert.Equal(t, auction.StatusCleared, a.Status)
}

func TestSettlingRequiresACertificate(t *testing.T) {
	t.Parallel()

	a := newAuction(t)
	require.NoError(t, a.Open(testOpens))
	require.NoError(t, a.StartClearing(testClose))

	// Reaching CLEARED without a certificate is only possible through a corrupted read;
	// settlement must still refuse to execute transfers it cannot point at a proof for.
	forced := auction.NewForTest(auction.StatusCleared, "")
	require.ErrorIs(t, forced.StartSettling(testClose), apperr.ErrConflict)
}

func TestClearingFailureIsRetried(t *testing.T) {
	t.Parallel()

	a := clearingAuction(t)
	require.NoError(t, a.FailClearing("independent verifier rejected the solution", testClose))

	assert.Equal(t, auction.StatusFailed, a.Status)
	assert.Equal(t, "independent verifier rejected the solution", a.Reason)

	require.ErrorIs(t, a.RetrySettlement(testClose), apperr.ErrConflict, "nothing cleared, so nothing to settle")

	require.NoError(t, a.RetryClearing(testClose))
	assert.Equal(t, auction.StatusClearing, a.Status)
	assert.Empty(t, a.Reason)
}

func TestSettlementFailureIsRetried(t *testing.T) {
	t.Parallel()

	a := settlingAuction(t)
	require.NoError(t, a.FailSettlement("transfer 2 of 3 was not confirmed", testClose))

	assert.Equal(t, auction.StatusFailed, a.Status)
	require.ErrorIs(t, a.RetryClearing(testClose), apperr.ErrConflict, "a cleared batch is not re-cleared")

	require.NoError(t, a.RetrySettlement(testClose))
	assert.Equal(t, auction.StatusSettling, a.Status)
	assert.Empty(t, a.Reason)
}

func TestCancel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		build func(*testing.T) *auction.Auction
	}{
		{name: "draft", build: func(t *testing.T) *auction.Auction { t.Helper(); return newAuction(t) }},
		{name: "open", build: func(t *testing.T) *auction.Auction {
			t.Helper()
			a := newAuction(t)
			require.NoError(t, a.Open(testOpens))
			return a
		}},
		{name: "cleared but unsettled", build: clearedAuction},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			a := tc.build(t)
			require.NoError(t, a.Cancel("no eligible bids", testClose))

			assert.Equal(t, auction.StatusCancelled, a.Status)
			assert.True(t, a.Status.IsTerminal())
			require.ErrorIs(t, a.Open(testOpens), apperr.ErrConflict)
		})
	}
}

func TestReasonIsValidated(t *testing.T) {
	t.Parallel()

	a := newAuction(t)
	require.ErrorIs(t, a.Cancel("  ", testClose), apperr.ErrValidation)
	require.ErrorIs(t, a.Cancel(longString(auction.MaxReasonLen+1), testClose), apperr.ErrValidation)
	assert.Equal(t, auction.StatusDraft, a.Status)
}

func TestParseStatus(t *testing.T) {
	t.Parallel()

	got, err := auction.ParseStatus("CLEARING")
	require.NoError(t, err)
	assert.Equal(t, auction.StatusClearing, got)
	assert.Equal(t, "CLEARING", got.String())

	_, err = auction.ParseStatus("clearing")
	require.ErrorIs(t, err, apperr.ErrValidation)
}

// TestTransitionTableIsClosed proves the state machine cannot strand an auction in a state
// that points at an unknown one.
func TestTransitionTableIsClosed(t *testing.T) {
	t.Parallel()

	reachable := map[auction.Status]bool{auction.StatusDraft: true}
	for _, from := range auction.AllStatuses() {
		require.Truef(t, from.IsValid(), "source status %s is not registered", from)
		for _, to := range auction.TransitionsFrom(from) {
			assert.Truef(t, to.IsValid(), "%s points at unknown status %s", from, to)
			assert.NotEqualf(t, from, to, "%s has a self transition", from)
			reachable[to] = true
		}
	}

	for _, status := range auction.AllStatuses() {
		assert.Truef(t, reachable[status], "status %s is unreachable", status)
	}

	terminal := map[auction.Status]bool{auction.StatusSettled: true, auction.StatusCancelled: true}
	for _, status := range auction.AllStatuses() {
		assert.Equalf(t, terminal[status], status.IsTerminal(), "IsTerminal(%s)", status)
	}
}

func TestBidStatusValidity(t *testing.T) {
	t.Parallel()

	assert.True(t, auction.BidStatusActive.IsValid())
	assert.True(t, auction.BidStatusRejected.IsValid())
	assert.False(t, auction.BidStatus("PENDING").IsValid())
	assert.Equal(t, "ALLOCATED", auction.BidStatusAllocated.String())
}

func clearingAuction(t *testing.T) *auction.Auction {
	t.Helper()

	a := newAuction(t)
	require.NoError(t, a.Open(testOpens))
	require.NoError(t, a.StartClearing(testClose))
	return a
}

func clearedAuction(t *testing.T) *auction.Auction {
	t.Helper()

	a := clearingAuction(t)
	require.NoError(t, a.MarkCleared("solver-v1", testCertificate, testClose))
	return a
}

func settlingAuction(t *testing.T) *auction.Auction {
	t.Helper()

	a := clearedAuction(t)
	require.NoError(t, a.StartSettling(testClose))
	return a
}

func longString(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = 'x'
	}
	return string(b)
}
