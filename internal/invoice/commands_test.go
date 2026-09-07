package invoice_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoldFridge/factorflow/internal/invoice"
	"github.com/GoldFridge/factorflow/internal/platform/apperr"
)

// advance returns a timestamp after the previous mutation, so version and timestamp
// changes are distinguishable.
func advance(step int) time.Time { return testNow.Add(time.Duration(step) * time.Minute) }

func TestHappyPathToMatured(t *testing.T) {
	t.Parallel()

	inv := newDraft(t)
	assessmentID, assetID := uuid.New(), uuid.New()

	require.NoError(t, inv.MarkUploaded(advance(1)))
	assert.Equal(t, invoice.StatusUploaded, inv.Status)

	require.NoError(t, inv.StartAssessment(advance(2)))
	require.NoError(t, inv.CompleteAssessment(assessmentID, advance(3)))
	assert.Equal(t, assessmentID, inv.AssessmentID)

	require.NoError(t, inv.Approve(advance(4)))
	require.NoError(t, inv.StartTokenization(advance(5)))
	require.NoError(t, inv.CompleteTokenization(assetID, advance(6)))
	assert.Equal(t, assetID, inv.AssetID)

	require.NoError(t, inv.OpenAuction(advance(7)))
	require.NoError(t, inv.MarkAllocated(advance(8)))
	require.NoError(t, inv.MarkSettled(advance(9)))
	require.NoError(t, inv.MarkMatured(advance(10)))

	assert.Equal(t, invoice.StatusMatured, inv.Status)
	assert.True(t, inv.Status.IsTerminal())
	assert.Equal(t, int64(11), inv.Version, "every transition bumps the version exactly once")
	assert.Equal(t, advance(10), inv.UpdatedAt)
	assert.Equal(t, testNow, inv.CreatedAt, "creation time never moves")
}

func TestSettledCanDefault(t *testing.T) {
	t.Parallel()

	inv := settledInvoice(t)
	require.NoError(t, inv.MarkDefaulted("debtor missed the due date", advance(20)))

	assert.Equal(t, invoice.StatusDefaulted, inv.Status)
	assert.Equal(t, "debtor missed the due date", inv.Reason)
	assert.True(t, inv.Status.IsTerminal())

	require.ErrorIs(t, inv.MarkMatured(advance(21)), apperr.ErrConflict)
}

func TestTransitionsOutOfOrderAreConflicts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		call func(*invoice.Invoice) error
	}{
		{name: "assess a draft", call: func(i *invoice.Invoice) error { return i.StartAssessment(advance(1)) }},
		{name: "approve a draft", call: func(i *invoice.Invoice) error { return i.Approve(advance(1)) }},
		{name: "tokenize a draft", call: func(i *invoice.Invoice) error { return i.StartTokenization(advance(1)) }},
		{name: "open an auction on a draft", call: func(i *invoice.Invoice) error { return i.OpenAuction(advance(1)) }},
		{name: "allocate a draft", call: func(i *invoice.Invoice) error { return i.MarkAllocated(advance(1)) }},
		{name: "settle a draft", call: func(i *invoice.Invoice) error { return i.MarkSettled(advance(1)) }},
		{name: "mature a draft", call: func(i *invoice.Invoice) error { return i.MarkMatured(advance(1)) }},
		{name: "default a draft", call: func(i *invoice.Invoice) error { return i.MarkDefaulted("no", advance(1)) }},
		{name: "cancel an auction that never opened", call: func(i *invoice.Invoice) error { return i.CancelAuction("no", advance(1)) }},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			inv := newDraft(t)
			err := tc.call(inv)

			require.ErrorIs(t, err, apperr.ErrConflict)
			assert.Equal(t, invoice.StatusDraft, inv.Status, "a rejected command leaves the state untouched")
			assert.Equal(t, int64(1), inv.Version, "a rejected command does not bump the version")
		})
	}
}

func TestApproveRequiresAnAssessment(t *testing.T) {
	t.Parallel()

	inv := newDraft(t)
	require.NoError(t, inv.MarkUploaded(advance(1)))
	require.NoError(t, inv.StartAssessment(advance(2)))

	// Reaching ASSESSED without recording the assessment id is only possible through a
	// corrupted read; approval must still refuse to price an unassessed receivable.
	require.ErrorIs(t, invoice.NewForTest(invoice.StatusAssessed, uuid.Nil).Approve(advance(3)), apperr.ErrConflict)

	require.ErrorIs(t, inv.CompleteAssessment(uuid.Nil, advance(3)), apperr.ErrValidation)
	assert.Equal(t, invoice.StatusExtracting, inv.Status)

	require.NoError(t, inv.CompleteAssessment(uuid.New(), advance(4)))
	require.NoError(t, inv.Approve(advance(5)))
}

func TestCompleteTokenizationRequiresAssetID(t *testing.T) {
	t.Parallel()

	inv := approvedInvoice(t)
	require.NoError(t, inv.StartTokenization(advance(5)))

	require.ErrorIs(t, inv.CompleteTokenization(uuid.Nil, advance(6)), apperr.ErrValidation)
	assert.Equal(t, invoice.StatusTokenizing, inv.Status)
}

func TestAssessmentFailureRetriesOnlyItsOwnStage(t *testing.T) {
	t.Parallel()

	inv := newDraft(t)
	require.NoError(t, inv.MarkUploaded(advance(1)))
	require.NoError(t, inv.StartAssessment(advance(2)))
	require.NoError(t, inv.FailAssessment("TEE did not respond after 3 attempts", advance(3)))

	assert.Equal(t, invoice.StatusFailed, inv.Status)
	assert.Equal(t, invoice.StatusExtracting, inv.FailedFrom)
	assert.Equal(t, "TEE did not respond after 3 attempts", inv.Reason)

	// A failure during extraction must not be resumable as issuance.
	require.ErrorIs(t, inv.StartTokenization(advance(4)), apperr.ErrConflict)
	assert.Equal(t, invoice.StatusFailed, inv.Status)

	require.NoError(t, inv.StartAssessment(advance(5)))
	assert.Equal(t, invoice.StatusExtracting, inv.Status)
	assert.Empty(t, inv.FailedFrom, "a successful retry clears the failure")
	assert.Empty(t, inv.Reason)
}

func TestTokenizationFailureRetriesOnlyItsOwnStage(t *testing.T) {
	t.Parallel()

	inv := approvedInvoice(t)
	require.NoError(t, inv.StartTokenization(advance(5)))
	require.NoError(t, inv.FailTokenization("ATS factory call reverted", advance(6)))

	assert.Equal(t, invoice.StatusTokenizing, inv.FailedFrom)
	require.ErrorIs(t, inv.StartAssessment(advance(7)), apperr.ErrConflict)

	require.NoError(t, inv.StartTokenization(advance(8)))
	assert.Equal(t, invoice.StatusTokenizing, inv.Status)
}

func TestSettlementFailureIsRetriedFromAllocated(t *testing.T) {
	t.Parallel()

	inv := allocatedInvoice(t)
	require.NoError(t, inv.FailSettlement("transfer 2 of 3 was not confirmed", advance(9)))

	assert.Equal(t, invoice.StatusFailed, inv.Status)
	assert.Equal(t, invoice.StatusAllocated, inv.FailedFrom)

	require.NoError(t, inv.MarkAllocated(advance(10)))
	require.NoError(t, inv.MarkSettled(advance(11)))
	assert.Equal(t, invoice.StatusSettled, inv.Status)
}

func TestCancelAuctionReturnsAssetToTokenized(t *testing.T) {
	t.Parallel()

	inv := tokenizedInvoice(t)
	require.NoError(t, inv.OpenAuction(advance(7)))
	require.NoError(t, inv.CancelAuction("no eligible bids", advance(8)))

	assert.Equal(t, invoice.StatusTokenized, inv.Status)
	assert.Equal(t, "no eligible bids", inv.Reason)

	require.NoError(t, inv.OpenAuction(advance(9)), "a cancelled asset can be auctioned again")
}

func TestRejectPaths(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		build func(*testing.T) *invoice.Invoice
	}{
		{name: "from draft", build: newDraft},
		{name: "from uploaded", build: func(t *testing.T) *invoice.Invoice {
			t.Helper()
			inv := newDraft(t)
			require.NoError(t, inv.MarkUploaded(advance(1)))
			return inv
		}},
		{name: "from assessed", build: assessedInvoice},
		{name: "from failed", build: func(t *testing.T) *invoice.Invoice {
			t.Helper()
			inv := newDraft(t)
			require.NoError(t, inv.MarkUploaded(advance(1)))
			require.NoError(t, inv.StartAssessment(advance(2)))
			require.NoError(t, inv.FailAssessment("invalid schema", advance(3)))
			return inv
		}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			inv := tc.build(t)
			require.NoError(t, inv.Reject("synthetic demo rejection", advance(30)))

			assert.Equal(t, invoice.StatusRejected, inv.Status)
			assert.True(t, inv.Status.IsTerminal())
			require.ErrorIs(t, inv.StartAssessment(advance(31)), apperr.ErrConflict)
		})
	}
}

func TestReasonIsValidated(t *testing.T) {
	t.Parallel()

	inv := newDraft(t)
	require.ErrorIs(t, inv.Reject("   ", advance(1)), apperr.ErrValidation)
	require.ErrorIs(t, inv.Reject(longString(invoice.MaxReasonLen+1), advance(1)), apperr.ErrValidation)
	assert.Equal(t, invoice.StatusDraft, inv.Status)

	require.NoError(t, inv.Reject("  duplicate submission  ", advance(2)))
	assert.Equal(t, "duplicate submission", inv.Reason, "reasons are trimmed")
}

func assessedInvoice(t *testing.T) *invoice.Invoice {
	t.Helper()
	inv := newDraft(t)
	require.NoError(t, inv.MarkUploaded(advance(1)))
	require.NoError(t, inv.StartAssessment(advance(2)))
	require.NoError(t, inv.CompleteAssessment(uuid.New(), advance(3)))
	return inv
}

func approvedInvoice(t *testing.T) *invoice.Invoice {
	t.Helper()
	inv := assessedInvoice(t)
	require.NoError(t, inv.Approve(advance(4)))
	return inv
}

func tokenizedInvoice(t *testing.T) *invoice.Invoice {
	t.Helper()
	inv := approvedInvoice(t)
	require.NoError(t, inv.StartTokenization(advance(5)))
	require.NoError(t, inv.CompleteTokenization(uuid.New(), advance(6)))
	return inv
}

func allocatedInvoice(t *testing.T) *invoice.Invoice {
	t.Helper()
	inv := tokenizedInvoice(t)
	require.NoError(t, inv.OpenAuction(advance(7)))
	require.NoError(t, inv.MarkAllocated(advance(8)))
	return inv
}

func settledInvoice(t *testing.T) *invoice.Invoice {
	t.Helper()
	inv := allocatedInvoice(t)
	require.NoError(t, inv.MarkSettled(advance(9)))
	return inv
}
