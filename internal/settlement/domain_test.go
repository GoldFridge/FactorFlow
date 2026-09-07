package settlement_test

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoldFridge/factorflow/internal/platform/apperr"
	"github.com/GoldFridge/factorflow/internal/platform/money"
	"github.com/GoldFridge/factorflow/internal/settlement"
)

var testNow = time.Date(2026, time.September, 8, 10, 0, 0, 0, time.UTC)

func params() settlement.NewParams {
	return settlement.NewParams{
		ID:         uuid.MustParse("11111111-1111-4111-8111-111111111111"),
		AuctionID:  uuid.MustParse("22222222-2222-4222-8222-222222222222"),
		LotID:      uuid.MustParse("33333333-3333-4333-8333-333333333333"),
		BidID:      uuid.MustParse("44444444-4444-4444-8444-444444444444"),
		InvoiceID:  uuid.MustParse("55555555-5555-4555-8555-555555555555"),
		AssetID:    uuid.MustParse("66666666-6666-4666-8666-666666666666"),
		InvestorID: uuid.MustParse("77777777-7777-4777-8777-777777777777"),
		FromWallet: "0x1111111111111111111111111111111111111111",
		ToWallet:   "0x2222222222222222222222222222222222222222",
		Notional:   money.MustParse("10000.00", money.USD),
		Price:      money.MustParse("9755.32", money.USD),
	}
}

func newSettlement(t *testing.T) *settlement.Settlement {
	t.Helper()

	s, err := settlement.New(params(), testNow)
	require.NoError(t, err)
	return s
}

func TestNewPlansATransfer(t *testing.T) {
	t.Parallel()

	s := newSettlement(t)

	assert.Equal(t, settlement.StatePrepared, s.State)
	assert.Zero(t, s.Attempts)
	assert.Empty(t, s.TxID, "nothing has been sent yet")
	assert.Equal(t, int64(1), s.Version)
	assert.Regexp(t, `^0x[0-9a-f]{64}$`, s.OperationID)
}

// TestTheOperationIdIsDerivedFromThePlan is what makes a resubmission recognisable: the
// same transfer computes the same id, and a different one does not.
func TestTheOperationIdIsDerivedFromThePlan(t *testing.T) {
	t.Parallel()

	first := newSettlement(t)

	// A second plan for the same allocation, created later and with a different row id.
	second, err := settlement.New(func() settlement.NewParams {
		p := params()
		p.ID = uuid.New()
		return p
	}(), testNow.Add(time.Hour))
	require.NoError(t, err)

	assert.Equal(t, first.OperationID, second.OperationID,
		"the id names the transfer, not the attempt")

	tests := []struct {
		name   string
		mutate func(*settlement.NewParams)
	}{
		{name: "another lot", mutate: func(p *settlement.NewParams) { p.LotID = uuid.New() }},
		{name: "another bid", mutate: func(p *settlement.NewParams) { p.BidID = uuid.New() }},
		{name: "another recipient", mutate: func(p *settlement.NewParams) {
			p.ToWallet = "0x3333333333333333333333333333333333333333"
		}},
		{name: "another amount", mutate: func(p *settlement.NewParams) {
			p.Notional = money.MustParse("9000.00", money.USD)
		}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := params()
			tc.mutate(&p)
			other, err := settlement.New(p, testNow)
			require.NoError(t, err)
			assert.NotEqual(t, first.OperationID, other.OperationID)
		})
	}
}

func TestNewValidatesThePlan(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*settlement.NewParams)
	}{
		{name: "no id", mutate: func(p *settlement.NewParams) { p.ID = uuid.Nil }},
		{name: "no auction", mutate: func(p *settlement.NewParams) { p.AuctionID = uuid.Nil }},
		{name: "no asset", mutate: func(p *settlement.NewParams) { p.AssetID = uuid.Nil }},
		{name: "no sender", mutate: func(p *settlement.NewParams) { p.FromWallet = "  " }},
		{name: "no recipient", mutate: func(p *settlement.NewParams) { p.ToWallet = "" }},
		{name: "sender is the recipient", mutate: func(p *settlement.NewParams) { p.ToWallet = p.FromWallet }},
		{name: "zero notional", mutate: func(p *settlement.NewParams) { p.Notional = money.Zero(money.USD) }},
		{name: "zero price", mutate: func(p *settlement.NewParams) { p.Price = money.Zero(money.USD) }},
		{name: "mixed currencies", mutate: func(p *settlement.NewParams) {
			p.Price = money.MustParse("9755.32", money.EUR)
		}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := params()
			tc.mutate(&p)

			_, err := settlement.New(p, testNow)
			require.ErrorIs(t, err, apperr.ErrValidation)
		})
	}
}

func TestWalletsAreNormalized(t *testing.T) {
	t.Parallel()

	p := params()
	p.FromWallet = "  0xAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA "
	p.ToWallet = "0xBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"

	s, err := settlement.New(p, testNow)
	require.NoError(t, err)
	assert.Equal(t, strings.ToLower(strings.TrimSpace(p.FromWallet)), s.FromWallet)
	assert.Equal(t, strings.ToLower(p.ToWallet), s.ToWallet)
}

// TestTheSagaRunsForwards walks the whole path a successful transfer takes.
func TestTheSagaRunsForwards(t *testing.T) {
	t.Parallel()

	s := newSettlement(t)

	require.NoError(t, s.Submit("0.0.1234@1757328000.000000001", testNow.Add(time.Second)))
	assert.Equal(t, settlement.StateSubmitted, s.State)
	assert.Equal(t, 1, s.Attempts)
	assert.True(t, s.IsSubmitted())

	require.NoError(t, s.ConfirmConsensus(testNow.Add(2*time.Second)))
	require.NoError(t, s.ConfirmMirror(testNow.Add(3*time.Second)))
	require.NoError(t, s.Account(testNow.Add(4*time.Second)))

	assert.Equal(t, settlement.StateAccounted, s.State)
	assert.True(t, s.IsFinished())
	assert.Equal(t, int64(5), s.Version, "every step is a stored version")
}

// TestASubmittedTransferCanNeverBeUnstarted is the rule the whole design protects: once a
// transaction exists, the plan must never look unstarted again, because a second
// submission would move the asset twice.
func TestASubmittedTransferCanNeverBeUnstarted(t *testing.T) {
	t.Parallel()

	s := newSettlement(t)
	require.NoError(t, s.Submit("tx-1", testNow))

	require.NoError(t, s.Fail("mirror node did not answer", testNow.Add(time.Minute)))
	assert.Equal(t, settlement.StateFailed, s.State)
	assert.True(t, s.IsSubmitted(), "the transaction still exists")
	assert.Equal(t, "tx-1", s.TxID)

	for _, next := range []settlement.State{settlement.StatePrepared} {
		assert.False(t, s.State.CanTransitionTo(next), "a failed settlement cannot be replanned")
	}

	// Resuming asks the chain instead of resubmitting: confirmation is reachable, and the
	// attempt count does not move.
	require.NoError(t, s.ConfirmConsensus(testNow.Add(2*time.Minute)))
	assert.Equal(t, 1, s.Attempts)
}

// TestAFailureBeforeSubmissionCannotBeConfirmed keeps the resume path honest: there is no
// transaction to confirm, so the only way forward is to submit one.
func TestAFailureBeforeSubmissionCannotBeConfirmed(t *testing.T) {
	t.Parallel()

	s := newSettlement(t)
	require.NoError(t, s.Fail("the issuer wallet is frozen", testNow))

	require.ErrorIs(t, s.ConfirmConsensus(testNow.Add(time.Minute)), apperr.ErrConflict)
	require.ErrorIs(t, s.Account(testNow.Add(time.Minute)), apperr.ErrConflict)

	require.NoError(t, s.Submit("tx-2", testNow.Add(2*time.Minute)))
	assert.Equal(t, 1, s.Attempts)
	assert.Empty(t, s.LastError, "a fresh submission clears the last cause")
}

func TestStepsCannotBeSkipped(t *testing.T) {
	t.Parallel()

	s := newSettlement(t)

	require.ErrorIs(t, s.ConfirmConsensus(testNow), apperr.ErrConflict)
	require.ErrorIs(t, s.Account(testNow), apperr.ErrConflict)

	require.NoError(t, s.Submit("tx-1", testNow))
	require.ErrorIs(t, s.ConfirmMirror(testNow), apperr.ErrConflict,
		"consensus comes before an independent read confirms it")
}

func TestSubmitNeedsATransactionId(t *testing.T) {
	t.Parallel()

	s := newSettlement(t)

	require.ErrorIs(t, s.Submit("   ", testNow), apperr.ErrValidation)
	assert.Equal(t, settlement.StatePrepared, s.State, "an unrecorded submission is not a submission")
	assert.Zero(t, s.Attempts)
}

func TestAnAccountedSettlementIsFinished(t *testing.T) {
	t.Parallel()

	s := newSettlement(t)
	require.NoError(t, s.Submit("tx-1", testNow))
	require.NoError(t, s.ConfirmConsensus(testNow))
	require.NoError(t, s.ConfirmMirror(testNow))
	require.NoError(t, s.Account(testNow))

	require.ErrorIs(t, s.Fail("too late", testNow), apperr.ErrConflict)
	assert.True(t, settlement.StateAccounted.IsFinished())
	assert.False(t, settlement.StateFailed.IsFinished(), "a failure still needs reconciling")
}

func TestFailNeedsACause(t *testing.T) {
	t.Parallel()

	s := newSettlement(t)
	require.ErrorIs(t, s.Fail("  ", testNow), apperr.ErrValidation)

	require.NoError(t, s.Fail(strings.Repeat("x", settlement.MaxErrorLen+50), testNow))
	assert.Len(t, s.LastError, settlement.MaxErrorLen, "an operator note is bounded")
}

func TestParseState(t *testing.T) {
	t.Parallel()

	state, err := settlement.ParseState("MIRROR_CONFIRMED")
	require.NoError(t, err)
	assert.Equal(t, settlement.StateMirrorConfirmed, state)

	_, err = settlement.ParseState("SETTLED")
	require.ErrorIs(t, err, apperr.ErrValidation, "a state outside the saga is not a state")
}
