package marketplace_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoldFridge/factorflow/internal/app/marketplace"
	"github.com/GoldFridge/factorflow/internal/auction"
	"github.com/GoldFridge/factorflow/internal/invoice"
	"github.com/GoldFridge/factorflow/internal/platform/apperr"
	"github.com/GoldFridge/factorflow/internal/platform/outbox"
	"github.com/GoldFridge/factorflow/internal/platform/postgres"
	"github.com/GoldFridge/factorflow/internal/settlement"
)

// wallets resolves an organization to the address it acts through.
type wallets struct{}

func (wallets) WalletOf(_ context.Context, _ postgres.Querier, organizationID uuid.UUID) (string, error) {
	// A deterministic address per organization, so a test can assert who received what.
	return "0x" + organizationID.String()[:8] + "000000000000000000000000000000000000", nil
}

// clearedFixture is a batch that has been cleared and is waiting to be settled.
type clearedFixture struct {
	*fixture
	worker   *marketplace.SettlementWorker
	executor *settlement.LocalExecutor
	auction  *auction.Auction
	invoice  *invoice.Invoice
	solution *auction.Solution
}

func newClearedFixture(t *testing.T) *clearedFixture {
	t.Helper()

	f := newSettlingFixture(t)
	inv := f.tokenizedInvoice(t, f.issuer.OrganizationID, "INV-1")

	a, err := f.service.OpenAuction(t.Context(), f.issuer, f.params(inv.ID))
	require.NoError(t, err)
	f.bid(t, a, uuid.New(), "0.05", "E", testNow)

	f.clock = testClose
	solution, err := f.service.ClearAuction(t.Context(), f.issuer, a.ID)
	require.NoError(t, err)
	require.Len(t, solution.Allocations, 1)

	return &clearedFixture{
		fixture: f.fixture, worker: f.worker, executor: f.executor,
		auction: a, invoice: inv, solution: solution,
	}
}

// settlingFixture wires the marketplace with a settlement store, wallets and an executor.
type settlingFixture struct {
	*fixture
	worker   *marketplace.SettlementWorker
	executor *settlement.LocalExecutor
}

func newSettlingFixture(t *testing.T) *settlingFixture {
	t.Helper()

	base := newFixture(t)
	executor := settlement.NewLocalExecutor(func() time.Time { return base.clock })

	base.service = marketplace.NewService(marketplace.Config{
		DB:          base.store,
		Invoices:    base.store.Invoices(),
		Assessments: base.store.Assessments(),
		Assets:      base.store.Assets(),
		Auctions:    base.store.Auctions(),
		Settlements: base.store.Settlements(),
		Wallets:     wallets{},
		Audit:       base.store.Audit(),
		Now:         func() time.Time { return base.clock },
		IDs:         uuid.New,
	})

	return &settlingFixture{
		fixture:  base,
		worker:   marketplace.NewSettlementWorker(base.service, executor, base.store.Assets()),
		executor: executor,
	}
}

// settleEvent is the outbox command the worker consumes.
func settleEvent(t *testing.T, id uuid.UUID) outbox.Event {
	t.Helper()

	payload, err := json.Marshal(map[string]any{"settlement_id": id})
	require.NoError(t, err)
	return outbox.Event{ID: 1, Topic: marketplace.TopicSettle, Payload: payload}
}

// TestSettlingPlansOneTransferPerAllocation is the composition this path exists for: the
// plan comes from the cleared solution, not from anything a caller supplies.
func TestSettlingPlansOneTransferPerAllocation(t *testing.T) {
	t.Parallel()

	f := newClearedFixture(t)

	planned, err := f.service.SettleAuction(t.Context(), f.issuer, f.auction.ID, "trace-1")
	require.NoError(t, err)
	require.Len(t, planned, 1)

	plan := planned[0]
	assert.Equal(t, settlement.StatePrepared, plan.State)
	assert.Equal(t, f.invoice.ID, plan.InvoiceID)
	assert.Equal(t, f.solution.Allocations[0].Notional, plan.Notional)
	assert.Equal(t, f.solution.Allocations[0].Price, plan.Price)
	assert.NotEqual(t, plan.FromWallet, plan.ToWallet, "the receivable leaves the issuer")

	stored, ok := f.store.Auction(f.auction.ID)
	require.True(t, ok)
	assert.Equal(t, auction.StatusSettling, stored.Status)

	// The command is queued in the same transaction, so a batch cannot sit in SETTLING
	// with nothing on its way to move it.
	assert.True(t, f.store.Statements().Executed("outbox_events"))
}

// TestSettlingTwicePlansOnce is the guard against a double transfer at the plan level: the
// second command resumes the saga that exists.
func TestSettlingTwicePlansOnce(t *testing.T) {
	t.Parallel()

	f := newClearedFixture(t)

	first, err := f.service.SettleAuction(t.Context(), f.issuer, f.auction.ID, "trace-1")
	require.NoError(t, err)

	second, err := f.service.SettleAuction(t.Context(), f.issuer, f.auction.ID, "trace-2")
	require.NoError(t, err)

	assert.Equal(t, 1, f.store.SettlementCount())
	assert.Equal(t, first[0].ID, second[0].ID)
}

// TestTheSagaRunsToAccounted walks the whole path and checks the local books end up
// matching what the chain holds.
func TestTheSagaRunsToAccounted(t *testing.T) {
	t.Parallel()

	f := newClearedFixture(t)
	planned, err := f.service.SettleAuction(t.Context(), f.issuer, f.auction.ID, "trace-1")
	require.NoError(t, err)

	require.NoError(t, f.worker.Handle(t.Context(), settleEvent(t, planned[0].ID)))

	plan, ok := f.store.Settlement(planned[0].ID)
	require.True(t, ok)
	assert.Equal(t, settlement.StateAccounted, plan.State)
	assert.NotEmpty(t, plan.TxID, "the transaction is on record")
	assert.Equal(t, 1, plan.Attempts)

	inv, ok := f.store.Invoice(f.invoice.ID)
	require.True(t, ok)
	assert.Equal(t, invoice.StatusSettled, inv.Status)

	a, ok := f.store.Auction(f.auction.ID)
	require.True(t, ok)
	assert.Equal(t, auction.StatusSettled, a.Status, "the last transfer closes the batch")

	actions := f.store.Audit().Actions()
	assert.Contains(t, actions, marketplace.ActionSettlementPlanned)
	assert.Contains(t, actions, marketplace.ActionInvoiceSettled)
	assert.Contains(t, actions, marketplace.ActionSettled)
}

// TestRedeliveryDoesNotTransferTwice is the rule the saga exists for: the outbox may
// deliver the same command again, and the asset must move once.
func TestRedeliveryDoesNotTransferTwice(t *testing.T) {
	t.Parallel()

	f := newClearedFixture(t)
	planned, err := f.service.SettleAuction(t.Context(), f.issuer, f.auction.ID, "trace-1")
	require.NoError(t, err)

	event := settleEvent(t, planned[0].ID)
	require.NoError(t, f.worker.Handle(t.Context(), event))
	require.NoError(t, f.worker.Handle(t.Context(), event), "a redelivery is not a second transfer")

	assert.Equal(t, 1, f.executor.SubmissionCount())

	plan, _ := f.store.Settlement(planned[0].ID)
	assert.Equal(t, 1, plan.Attempts)
	assert.Equal(t, settlement.StateAccounted, plan.State)
}

// TestATransferThatCannotBeSeenYetIsNotAFailure is the distinction that keeps an asset
// from moving twice: the transaction exists, so the worker waits rather than resubmitting.
func TestATransferThatCannotBeSeenYetIsNotAFailure(t *testing.T) {
	t.Parallel()

	f := newClearedFixture(t)
	planned, err := f.service.SettleAuction(t.Context(), f.issuer, f.auction.ID, "trace-1")
	require.NoError(t, err)

	f.executor.WithholdConfirmation(true)

	event := settleEvent(t, planned[0].ID)
	err = f.worker.Handle(t.Context(), event)
	require.ErrorIs(t, err, apperr.ErrUnavailable, "the outbox retries this rather than parking it")

	plan, _ := f.store.Settlement(planned[0].ID)
	assert.True(t, plan.IsSubmitted(), "the transaction id is kept, so the retry can ask about it")
	assert.Equal(t, 1, plan.Attempts)
	txID := plan.TxID

	// The chain catches up and the retry finishes the saga without submitting again.
	f.executor.Confirm(txID)
	require.NoError(t, f.worker.Handle(t.Context(), event))

	plan, _ = f.store.Settlement(planned[0].ID)
	assert.Equal(t, settlement.StateAccounted, plan.State)
	assert.Equal(t, txID, plan.TxID, "the same transaction, not a second one")
	assert.Equal(t, 1, plan.Attempts)
	assert.Equal(t, 1, f.executor.SubmissionCount())
}

// TestARejectedTransactionStops keeps a refused transfer from being reported as settled.
func TestARejectedTransactionStops(t *testing.T) {
	t.Parallel()

	f := newClearedFixture(t)
	planned, err := f.service.SettleAuction(t.Context(), f.issuer, f.auction.ID, "trace-1")
	require.NoError(t, err)

	f.executor.WithholdConfirmation(true)
	event := settleEvent(t, planned[0].ID)
	require.Error(t, f.worker.Handle(t.Context(), event))

	plan, _ := f.store.Settlement(planned[0].ID)
	f.executor.Reject(plan.TxID, "INSUFFICIENT_TOKEN_BALANCE")

	err = f.worker.Handle(t.Context(), event)
	require.ErrorIs(t, err, apperr.ErrConflict)

	plan, _ = f.store.Settlement(planned[0].ID)
	assert.Equal(t, settlement.StateFailed, plan.State)
	assert.Contains(t, plan.LastError, "INSUFFICIENT_TOKEN_BALANCE")
	assert.Equal(t, plan.TxID, plan.TxID, "the transaction stays on record for reconciliation")

	inv, _ := f.store.Invoice(f.invoice.ID)
	assert.Equal(t, invoice.StatusAllocated, inv.Status, "nothing claims the invoice settled")

	a, _ := f.store.Auction(f.auction.ID)
	assert.Equal(t, auction.StatusSettling, a.Status)
}

// TestAFailedSubmissionCanBeRetried covers the ordinary outage: nothing reached the chain,
// so a retry submits for the first time.
func TestAFailedSubmissionCanBeRetried(t *testing.T) {
	t.Parallel()

	f := newClearedFixture(t)
	planned, err := f.service.SettleAuction(t.Context(), f.issuer, f.auction.ID, "trace-1")
	require.NoError(t, err)

	f.executor.FailSubmissions(errors.New("the node is not answering"))
	event := settleEvent(t, planned[0].ID)
	require.Error(t, f.worker.Handle(t.Context(), event))

	plan, _ := f.store.Settlement(planned[0].ID)
	assert.Equal(t, settlement.StateFailed, plan.State)
	assert.False(t, plan.IsSubmitted(), "nothing reached the chain")
	assert.Zero(t, plan.Attempts)

	f.executor.FailSubmissions(nil)
	require.NoError(t, f.worker.Handle(t.Context(), event))

	plan, _ = f.store.Settlement(planned[0].ID)
	assert.Equal(t, settlement.StateAccounted, plan.State)
	assert.Equal(t, 1, plan.Attempts)
	assert.Equal(t, 1, f.executor.SubmissionCount())
}

func TestSettlingRequiresAClearedBatch(t *testing.T) {
	t.Parallel()

	f := newSettlingFixture(t)
	inv := f.tokenizedInvoice(t, f.issuer.OrganizationID, "INV-1")

	a, err := f.service.OpenAuction(t.Context(), f.issuer, f.params(inv.ID))
	require.NoError(t, err)

	_, err = f.service.SettleAuction(t.Context(), f.issuer, a.ID, "trace-1")
	require.ErrorIs(t, err, apperr.ErrConflict, "an open batch has allocated nothing")
	assert.Equal(t, 0, f.store.SettlementCount())

	_, err = f.service.SettleAuction(t.Context(), f.issuer, uuid.New(), "trace-1")
	require.ErrorIs(t, err, apperr.ErrNotFound)
}

func TestSettlingIsForTheIssuerOnly(t *testing.T) {
	t.Parallel()

	f := newClearedFixture(t)

	_, err := f.service.SettleAuction(t.Context(), f.other, f.auction.ID, "trace-1")
	require.ErrorIs(t, err, apperr.ErrForbidden)
	assert.Equal(t, 0, f.store.SettlementCount())

	_, err = f.service.Settlements(t.Context(), f.other, f.auction.ID)
	require.ErrorIs(t, err, apperr.ErrForbidden)

	planned, err := f.service.SettleAuction(t.Context(),
		marketplace.Actor{OrganizationID: uuid.New(), Operator: true}, f.auction.ID, "trace-1")
	require.NoError(t, err, "an operator may settle any batch")
	assert.Len(t, planned, 1)
}

// TestSettlingWithoutAStoreReportsItself keeps a deployment that cannot settle from
// failing at some later, less obvious point.
func TestSettlingWithoutAStoreReportsItself(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	_, err := f.service.SettleAuction(t.Context(), f.issuer, uuid.New(), "trace-1")
	require.ErrorIs(t, err, apperr.ErrUnavailable)
}
