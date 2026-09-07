package settlement_test

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoldFridge/factorflow/internal/platform/apperr"
	"github.com/GoldFridge/factorflow/internal/platform/money"
	"github.com/GoldFridge/factorflow/internal/settlement"
)

func newExecutor() *settlement.LocalExecutor {
	return settlement.NewLocalExecutor(func() time.Time { return testNow })
}

func order(operationID string) settlement.Order {
	return settlement.Order{
		OperationID: operationID,
		Network:     settlement.LocalNetwork,
		TokenID:     "local-token-1",
		FromWallet:  "0x1111111111111111111111111111111111111111",
		ToWallet:    "0x2222222222222222222222222222222222222222",
		Notional:    money.MustParse("10000.00", money.USD),
	}
}

// TestTheSameOperationTransfersOnce is the property the executor exists to provide: the
// saga may submit the same order again after a crash, and the asset must move once.
func TestTheSameOperationTransfersOnce(t *testing.T) {
	t.Parallel()

	e := newExecutor()

	first, err := e.Submit(t.Context(), order("op-1"))
	require.NoError(t, err)
	assert.NotEmpty(t, first.TxID)
	assert.False(t, first.Duplicate)

	second, err := e.Submit(t.Context(), order("op-1"))
	require.NoError(t, err)
	assert.Equal(t, first.TxID, second.TxID, "the resubmission finds the transfer that exists")
	assert.True(t, second.Duplicate)
	assert.Equal(t, 1, e.SubmissionCount())

	other, err := e.Submit(t.Context(), order("op-2"))
	require.NoError(t, err)
	assert.NotEqual(t, first.TxID, other.TxID)
	assert.Equal(t, 2, e.SubmissionCount())
}

// TestTransactionIdsAreDeterministic keeps a demo reproducible: the same plan produces the
// same identifiers on every run.
func TestTransactionIdsAreDeterministic(t *testing.T) {
	t.Parallel()

	first, err := newExecutor().Submit(t.Context(), order("op-1"))
	require.NoError(t, err)

	second, err := newExecutor().Submit(t.Context(), order("op-1"))
	require.NoError(t, err)

	assert.Equal(t, first.TxID, second.TxID)
}

func TestLookupReportsWhatTheLedgerHolds(t *testing.T) {
	t.Parallel()

	e := newExecutor()
	receipt, err := e.Submit(t.Context(), order("op-1"))
	require.NoError(t, err)

	record, err := e.Lookup(t.Context(), receipt.TxID)
	require.NoError(t, err)
	assert.True(t, record.Found)
	assert.True(t, record.Succeeded)

	// A transaction nobody has heard of is not an error: it may not have propagated yet,
	// and the saga is what decides how long to keep asking.
	unknown, err := e.Lookup(t.Context(), "local-nothing")
	require.NoError(t, err)
	assert.False(t, unknown.Found)
	assert.False(t, unknown.Succeeded)
}

// TestAWithheldTransactionIsPendingNotFailed is the distinction the saga turns on: a
// transfer that cannot be seen yet must not be treated as one that failed.
func TestAWithheldTransactionIsPendingNotFailed(t *testing.T) {
	t.Parallel()

	e := newExecutor()
	e.WithholdConfirmation(true)

	receipt, err := e.Submit(t.Context(), order("op-1"))
	require.NoError(t, err)

	record, err := e.Lookup(t.Context(), receipt.TxID)
	require.NoError(t, err)
	assert.False(t, record.Found, "not visible yet")

	e.Confirm(receipt.TxID)
	record, err = e.Lookup(t.Context(), receipt.TxID)
	require.NoError(t, err)
	assert.True(t, record.Found)
	assert.True(t, record.Succeeded)
}

func TestARejectedTransactionIsFoundAndFailed(t *testing.T) {
	t.Parallel()

	e := newExecutor()
	receipt, err := e.Submit(t.Context(), order("op-1"))
	require.NoError(t, err)

	e.Reject(receipt.TxID, "INSUFFICIENT_TOKEN_BALANCE")

	record, err := e.Lookup(t.Context(), receipt.TxID)
	require.NoError(t, err)
	assert.True(t, record.Found, "the chain has an answer")
	assert.False(t, record.Succeeded, "and the answer is no")
	assert.Equal(t, "INSUFFICIENT_TOKEN_BALANCE", record.Status)
}

func TestFailuresAreDrivable(t *testing.T) {
	t.Parallel()

	e := newExecutor()
	boom := errors.New("the node is not answering")

	e.FailSubmissions(boom)
	_, err := e.Submit(t.Context(), order("op-1"))
	require.ErrorIs(t, err, boom)
	assert.Equal(t, 0, e.SubmissionCount(), "a refused submission moved nothing")

	e.FailSubmissions(nil)
	receipt, err := e.Submit(t.Context(), order("op-1"))
	require.NoError(t, err)

	e.FailLookups(boom)
	_, err = e.Lookup(t.Context(), receipt.TxID)
	require.ErrorIs(t, err, boom)
}

func TestOrdersAreValidated(t *testing.T) {
	t.Parallel()

	e := newExecutor()

	tests := []struct {
		name   string
		mutate func(*settlement.Order)
	}{
		{name: "no operation", mutate: func(o *settlement.Order) { o.OperationID = " " }},
		{name: "no sender", mutate: func(o *settlement.Order) { o.FromWallet = "" }},
		{name: "no recipient", mutate: func(o *settlement.Order) { o.ToWallet = "" }},
		{name: "zero notional", mutate: func(o *settlement.Order) { o.Notional = money.Zero(money.USD) }},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			o := order("op-1")
			tc.mutate(&o)

			_, err := e.Submit(t.Context(), o)
			require.ErrorIs(t, err, apperr.ErrValidation)
		})
	}
}
