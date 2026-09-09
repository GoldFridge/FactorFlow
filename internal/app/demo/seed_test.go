package demo_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoldFridge/factorflow/internal/app/assessment"
	"github.com/GoldFridge/factorflow/internal/app/collections"
	"github.com/GoldFridge/factorflow/internal/app/demo"
	"github.com/GoldFridge/factorflow/internal/app/issuance"
	"github.com/GoldFridge/factorflow/internal/app/marketplace"
	"github.com/GoldFridge/factorflow/internal/auction"
	"github.com/GoldFridge/factorflow/internal/invoice"
	"github.com/GoldFridge/factorflow/internal/marketdata"
	"github.com/GoldFridge/factorflow/internal/organization"
	"github.com/GoldFridge/factorflow/internal/platform/audit"
	"github.com/GoldFridge/factorflow/internal/platform/clock"
	"github.com/GoldFridge/factorflow/internal/platform/outbox"
	"github.com/GoldFridge/factorflow/internal/platform/pgtest"
	"github.com/GoldFridge/factorflow/internal/platform/postgres"
	"github.com/GoldFridge/factorflow/internal/redemption"
	"github.com/GoldFridge/factorflow/internal/risk"
	"github.com/GoldFridge/factorflow/internal/settlement"
	"github.com/GoldFridge/factorflow/internal/tokenization"
)

func TestMain(m *testing.M) {
	code := m.Run()
	pgtest.Shutdown()
	os.Exit(code)
}

// wire builds the same object graph the server does, which is the point of the test: the
// seed is only useful if it runs against the real services.
func wire(t *testing.T, db *postgres.DB) (*demo.Seeder, *marketplace.Service) {
	t.Helper()

	// The services and the seeder share one clock, which is what lets the seed put a
	// finished batch in the past without any rule being relaxed for it.
	clk := clock.At(time.Now().Add(-demo.SeedHistory))
	now := clk.Now
	// Derived identifiers, for the same reason the server's seed path uses them: the risk
	// grade follows from the invoice id, so random ids would make this test flaky.
	ids := demo.IDs()
	invoices := invoice.NewPostgresRepository()
	assessments := risk.NewPostgresRepository()
	snapshots := marketdata.NewPostgresRepository()
	assets := tokenization.NewPostgresRepository()
	auctions := auction.NewPostgresRepository()
	organizations := organization.NewPostgresRepository()
	settlements := settlement.NewPostgresRepository()
	trail := audit.NewPostgresRecorder()

	market := marketdata.NewService(
		marketdata.NewStaticProvider(marketdata.DemoMarkets()...), marketdata.NewNormalizer(), now)

	invoiceService := invoice.NewService(db, invoices, trail, now, ids)
	auctionService := auction.NewService(db, auctions, auction.NewSolver(), trail, now, ids)
	marketplaceService := marketplace.NewService(marketplace.Config{
		DB: db, Invoices: invoices, Assessments: assessments, Assets: assets,
		Auctions: auctions, Settlements: settlements, Wallets: wallets{repo: organizations},
		Solver: auction.NewSolver(), Audit: trail, Now: now, IDs: ids,
	})

	assessmentWorker := assessment.NewAssessmentWorker(assessment.WorkerConfig{
		DB: db, Invoices: invoices, Assessments: assessments, Snapshots: snapshots,
		Market: market, Workflow: risk.NewDeterministicWorkflow(), Model: risk.ModelV1(),
		Query: marketdata.DemoQuery(), Audit: trail, Now: now, IDs: ids,
	})
	issuanceWorker := issuance.NewWorker(issuance.Config{
		DB: db, Invoices: invoices, Assessments: assessments, Assets: assets,
		Wallets: wallets{repo: organizations}, Issuer: tokenization.NewLocalIssuer(),
		Audit: trail, Now: now, IDs: ids,
	})
	settlementWorker := marketplace.NewSettlementWorker(
		marketplaceService, settlement.NewLocalExecutor(now), assets)

	dispatcher := outbox.NewDispatcher(db, outbox.DefaultDispatcherConfig(), now)
	dispatcher.Register(invoice.TopicAssess, assessmentWorker.Handle)
	dispatcher.Register(invoice.TopicTokenize, issuanceWorker.Handle)
	dispatcher.Register(marketplace.TopicSettle, settlementWorker.Handle)

	collectionService := collections.NewService(collections.Config{
		DB: db, Invoices: invoices, Settlements: settlements,
		Repayments: redemption.NewPostgresRepository(), Audit: trail, Now: now, IDs: ids,
	})

	seeder := demo.NewSeeder(demo.Config{
		DB: db, Organizations: organizations, Invoices: invoiceService,
		Marketplace: marketplaceService, Collections: collectionService,
		Auctions: auctionService, Dispatcher: dispatcher, Clock: clk,
	})
	return seeder, marketplaceService
}

// wallets answers the one question the workers ask about an organization.
type wallets struct{ repo organization.Repository }

func (w wallets) WalletOf(ctx context.Context, q postgres.Querier, organizationID uuid.UUID) (string, error) {
	org, err := w.repo.Get(ctx, q, organizationID)
	if err != nil {
		return "", err
	}
	return org.Wallet, nil
}

// TestSeedProducesEveryStageOfTheLifecycle is what a demo needs from it: a receivable
// sitting at each stage a screen has to render, produced by the real services.
func TestSeedProducesEveryStageOfTheLifecycle(t *testing.T) {
	db := pgtest.New(t)
	ctx := context.Background()

	seeder, _ := wire(t, db)

	summary, err := seeder.Run(ctx)
	require.NoError(t, err)
	assert.False(t, summary.Skipped)
	assert.Equal(t, 5, summary.Organizations)
	assert.Equal(t, 4, summary.Invoices)
	assert.Equal(t, 2, summary.Auctions)
	assert.Equal(t, 4, summary.Bids)
	assert.Equal(t, 1, summary.Settlements)
	assert.Equal(t, 1, summary.Repayments)

	invoices := invoice.NewPostgresRepository()
	statuses := map[invoice.Status]int{}
	for _, issuer := range []uuid.UUID{demo.IssuerAID, demo.IssuerBID} {
		stored, err := invoices.ListByIssuer(ctx, db.Querier(), issuer, 50)
		require.NoError(t, err)
		for _, inv := range stored {
			statuses[inv.Status]++
		}
	}

	assert.Equal(t, 1, statuses[invoice.StatusDraft], "one receivable is still being prepared")
	assert.Equal(t, 1, statuses[invoice.StatusAssessed], "one is waiting for its issuer to approve the price")
	assert.Equal(t, 1, statuses[invoice.StatusAuctionOpen], "one is on the market")
	assert.Equal(t, 1, statuses[invoice.StatusMatured],
		"and one has been financed end to end, down to the debtor paying it")
}

/*
 * TestSeededRepaymentDividesWhatTheDebtorPaid closes the story the rest of the seed sets
 * up. A demo that stops at "somebody bought this" never shows the number an investor
 * actually buys for, so the finished receivable is paid and the money divided — by the
 * service an operator would use, not by figures written down here.
 */
func TestSeededRepaymentDividesWhatTheDebtorPaid(t *testing.T) {
	db := pgtest.New(t)
	ctx := context.Background()

	seeder, _ := wire(t, db)
	_, err := seeder.Run(ctx)
	require.NoError(t, err)

	repayments, err := redemption.NewPostgresRepository().
		ListForParty(ctx, db.Querier(), demo.InvestorAID, 10)
	require.NoError(t, err)
	require.Len(t, repayments, 1, "the investor that was allocated the batch was paid")

	paid := repayments[0]
	assert.False(t, paid.IsShortfall(), "the demo debtor pays what it owes")

	share, ok := paid.ShareOf(demo.InvestorAID)
	require.True(t, ok)
	assert.True(t, share.Amount.IsPositive())

	// Every unit the debtor paid was handed to somebody.
	total := share.Amount
	for _, other := range paid.Shares {
		if other.PartyID == demo.InvestorAID {
			continue
		}
		total, err = total.Add(other.Amount)
		require.NoError(t, err)
	}
	assert.Equal(t, paid.Amount, total)
}

// TestSeededAuctionsAreBiddableAndSettled checks the two batches a demo needs: one a judge
// can act on, and one that already shows the finished state.
func TestSeededAuctionsAreBiddableAndSettled(t *testing.T) {
	db := pgtest.New(t)
	ctx := context.Background()

	seeder, market := wire(t, db)
	_, err := seeder.Run(ctx)
	require.NoError(t, err)

	auctions := auction.NewPostgresRepository()

	open, err := auctions.ListAuctions(ctx, db.Querier(), auction.StatusOpen, 10)
	require.NoError(t, err)
	require.Len(t, open, 1, "one batch is left open to bid into")

	bids, err := auctions.ListBids(ctx, db.Querier(), open[0].ID)
	require.NoError(t, err)
	assert.Len(t, bids, 2, "with bids already on it, so the book is not empty")

	settled, err := auctions.ListAuctions(ctx, db.Querier(), auction.StatusSettled, 10)
	require.NoError(t, err)
	require.Len(t, settled, 1)

	// The settled batch went through the saga rather than being written as finished.
	transfers, err := market.Settlements(ctx,
		marketplace.Actor{OrganizationID: demo.IssuerAID}, settled[0].ID)
	require.NoError(t, err)
	require.Len(t, transfers, 1)
	assert.Equal(t, settlement.StateAccounted, transfers[0].State)
	assert.NotEmpty(t, transfers[0].TxID, "there is a transaction to point at")
}

// TestSeedingTwiceChangesNothing keeps a restarting demo server from ending up with two of
// everything, and from failing to start because the data is already there.
func TestSeedingTwiceChangesNothing(t *testing.T) {
	db := pgtest.New(t)
	ctx := context.Background()

	seeder, _ := wire(t, db)

	first, err := seeder.Run(ctx)
	require.NoError(t, err)
	require.False(t, first.Skipped)

	second, err := seeder.Run(ctx)
	require.NoError(t, err)
	assert.True(t, second.Skipped)

	invoices, err := invoice.NewPostgresRepository().ListByIssuer(ctx, db.Querier(), demo.IssuerAID, 50)
	require.NoError(t, err)
	assert.Len(t, invoices, 3, "the same three receivables, not six")
}

// TestSeededDataHasAnAuditTrail is what makes the demo demonstrable: every seeded
// transition is on the timeline, because it actually happened.
func TestSeededDataHasAnAuditTrail(t *testing.T) {
	db := pgtest.New(t)
	ctx := context.Background()

	seeder, _ := wire(t, db)
	_, err := seeder.Run(ctx)
	require.NoError(t, err)

	invoices, err := invoice.NewPostgresRepository().ListByStatus(ctx, db.Querier(), invoice.StatusMatured, 10)
	require.NoError(t, err)
	require.Len(t, invoices, 1)

	events, err := audit.NewPostgresRecorder().Timeline(ctx, db.Querier(),
		invoice.EntityType, invoices[0].ID.String(), 0)
	require.NoError(t, err)

	actions := map[string]bool{}
	for _, e := range events {
		actions[e.Action] = true
	}

	for _, expected := range []string{
		invoice.ActionCreated,
		invoice.ActionDocumentAttached,
		invoice.ActionAssessmentRequested,
		assessment.ActionAssessed,
		invoice.ActionApproved,
		issuance.ActionTokenized,
		invoice.ActionAuctionOpened,
		marketplace.ActionInvoiceAllocated,
		marketplace.ActionInvoiceSettled,
		collections.ActionRepaid,
	} {
		assert.True(t, actions[expected], "the timeline is missing %s", expected)
	}
}
