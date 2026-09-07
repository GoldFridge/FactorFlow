package settlement_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoldFridge/factorflow/internal/auction"
	"github.com/GoldFridge/factorflow/internal/invoice"
	"github.com/GoldFridge/factorflow/internal/organization"
	"github.com/GoldFridge/factorflow/internal/platform/apperr"
	"github.com/GoldFridge/factorflow/internal/platform/money"
	"github.com/GoldFridge/factorflow/internal/platform/pgtest"
	"github.com/GoldFridge/factorflow/internal/platform/postgres"
	"github.com/GoldFridge/factorflow/internal/risk"
	"github.com/GoldFridge/factorflow/internal/settlement"
	"github.com/GoldFridge/factorflow/internal/tokenization"
)

func TestMain(m *testing.M) {
	code := m.Run()
	pgtest.Shutdown()
	os.Exit(code)
}

// allocation is a settled-able allocation and everything the foreign keys require: an
// issuer, an investor, an invoice, its asset, an auction with a lot, and a bid.
type allocation struct {
	auctionID  uuid.UUID
	lotID      uuid.UUID
	bidID      uuid.UUID
	invoiceID  uuid.UUID
	assetID    uuid.UUID
	investorID uuid.UUID
	issuer     string
	investor   string
}

// seed writes the rows a settlement points at. It goes through the real repositories so a
// schema change that breaks them breaks this too.
func seed(t *testing.T, db *postgres.DB, seq int) allocation {
	t.Helper()

	ctx := context.Background()
	q := db.Querier()

	orgs := organization.NewPostgresRepository()
	issuer := newOrg(t, orgs, q, organization.TypeIssuer, seq)
	investor := newOrg(t, orgs, q, organization.TypeInvestor, seq+100)

	inv, err := invoice.New(invoice.NewParams{
		ID:        uuid.New(),
		IssuerID:  issuer.ID,
		DebtorRef: "ACME Logistics GmbH",
		Number:    fmt.Sprintf("INV-%04d", seq),
		Face:      money.MustParse("10000.00", money.USD),
		IssuedAt:  testNow,
		DueAt:     testNow.Add(60 * 24 * time.Hour),
	}, testNow)
	require.NoError(t, err)

	assetID := uuid.New()
	require.NoError(t, inv.MarkUploaded(testNow))
	require.NoError(t, inv.StartAssessment(testNow))
	require.NoError(t, inv.CompleteAssessment(uuid.New(), testNow))
	require.NoError(t, inv.Approve(testNow))
	require.NoError(t, inv.StartTokenization(testNow))
	require.NoError(t, inv.CompleteTokenization(assetID, testNow))
	require.NoError(t, invoice.NewPostgresRepository().Create(ctx, q, inv))

	asset, err := tokenization.New(tokenization.NewParams{
		ID: assetID, InvoiceID: inv.ID, IssuerID: issuer.ID,
		Network: tokenization.LocalNetwork, TokenID: fmt.Sprintf("local-token-%d", seq),
		Supply: inv.Face, ChainStatus: tokenization.StatusIssued,
	}, testNow)
	require.NoError(t, err)
	require.NoError(t, tokenization.NewPostgresRepository().Create(ctx, q, asset))

	a, err := auction.NewAuction(auction.NewAuctionParams{
		ID:       uuid.New(),
		IssuerID: issuer.ID,
		Lots: []auction.Lot{{
			ID: uuid.New(), InvoiceID: inv.ID, AssetID: assetID, IssuerID: issuer.ID,
			DebtorRef: "ACME Logistics GmbH", Supply: inv.Face,
			ReservePrice: money.MustParse("9755.32", money.USD),
			Grade:        risk.GradeB, TenorDays: 60,
		}},
		OpensAt:  testNow,
		ClosesAt: testNow.Add(2 * time.Hour),
	}, testNow)
	require.NoError(t, err)

	bid, err := auction.NewBid(auction.NewBidParams{
		ID: uuid.New(), AuctionID: a.ID, InvestorID: investor.ID,
		Budget: money.MustParse("50000.00", money.USD), MinYield: money.MustParseRate("0.05"),
		MaxGrade: risk.GradeE, MaxTenorDays: 120,
		MinimumLot: money.Zero(money.USD),
	}, testNow)
	require.NoError(t, err)

	auctions := auction.NewPostgresRepository()
	require.NoError(t, auctions.CreateAuction(ctx, q, a))
	require.NoError(t, auctions.CreateBid(ctx, q, bid))

	return allocation{
		auctionID: a.ID, lotID: a.Lots[0].ID, bidID: bid.ID,
		invoiceID: inv.ID, assetID: assetID, investorID: investor.ID,
		issuer: issuer.Wallet, investor: investor.Wallet,
	}
}

func newOrg(t *testing.T, repo organization.Repository, q postgres.Querier, orgType organization.Type, seq int) *organization.Organization {
	t.Helper()

	org, err := organization.New(organization.NewParams{
		ID: uuid.New(), Type: orgType, Name: fmt.Sprintf("Org %d", seq),
		Wallet: fmt.Sprintf("0x%040x", seq),
	}, testNow)
	require.NoError(t, err)
	require.NoError(t, org.Approve(testNow))
	require.NoError(t, repo.Create(context.Background(), q, org))
	return org
}

func plan(t *testing.T, a allocation) *settlement.Settlement {
	t.Helper()

	s, err := settlement.New(settlement.NewParams{
		ID: uuid.New(), AuctionID: a.auctionID, LotID: a.lotID, BidID: a.bidID,
		InvoiceID: a.invoiceID, AssetID: a.assetID, InvestorID: a.investorID,
		FromWallet: a.issuer, ToWallet: a.investor,
		Notional: money.MustParse("10000.00", money.USD),
		Price:    money.MustParse("9755.32", money.USD),
	}, testNow)
	require.NoError(t, err)
	return s
}

func TestSettlementRoundTrip(t *testing.T) {
	db := pgtest.New(t)
	ctx := context.Background()
	repo := settlement.NewPostgresRepository()

	s := plan(t, seed(t, db, 1))
	require.NoError(t, repo.Create(ctx, db.Querier(), s))

	got, err := repo.Get(ctx, db.Querier(), s.ID)
	require.NoError(t, err)

	assert.Equal(t, s.OperationID, got.OperationID)
	assert.Equal(t, s.FromWallet, got.FromWallet)
	assert.Equal(t, s.ToWallet, got.ToWallet)
	assert.Equal(t, "10000.00", got.Notional.String())
	assert.Equal(t, "9755.32", got.Price.String())
	assert.Equal(t, settlement.StatePrepared, got.State)
	assert.Zero(t, got.Attempts)

	byOperation, err := repo.GetByOperation(ctx, db.Querier(), s.OperationID)
	require.NoError(t, err)
	assert.Equal(t, s.ID, byOperation.ID)

	_, err = repo.Get(ctx, db.Querier(), uuid.New())
	require.ErrorIs(t, err, apperr.ErrNotFound)
	_, err = repo.GetByOperation(ctx, db.Querier(), "0xdeadbeef")
	require.ErrorIs(t, err, apperr.ErrNotFound)
}

// TestOneAllocationSettlesOnce is the constraint that stops a batch from being settled
// twice: the second plan for the same allocation cannot be written at all.
func TestOneAllocationSettlesOnce(t *testing.T) {
	db := pgtest.New(t)
	ctx := context.Background()
	repo := settlement.NewPostgresRepository()

	a := seed(t, db, 2)
	require.NoError(t, repo.Create(ctx, db.Querier(), plan(t, a)))

	err := repo.Create(ctx, db.Querier(), plan(t, a))
	require.ErrorIs(t, err, apperr.ErrConflict,
		"a duplicate plan is refused by the database, not merely by the code that meant well")
}

func TestSettlementOptimisticConcurrency(t *testing.T) {
	db := pgtest.New(t)
	ctx := context.Background()
	repo := settlement.NewPostgresRepository()

	s := plan(t, seed(t, db, 3))
	require.NoError(t, repo.Create(ctx, db.Querier(), s))

	first, err := repo.Get(ctx, db.Querier(), s.ID)
	require.NoError(t, err)
	second, err := repo.Get(ctx, db.Querier(), s.ID)
	require.NoError(t, err)

	// Two workers both believe they are the one submitting this transfer.
	require.NoError(t, first.Submit("tx-first", testNow))
	require.NoError(t, repo.Update(ctx, db.Querier(), first, first.Version-1))

	require.NoError(t, second.Submit("tx-second", testNow))
	err = repo.Update(ctx, db.Querier(), second, second.Version-1)
	require.ErrorIs(t, err, apperr.ErrConflict, "the loser is told, rather than overwriting a live transfer")

	stored, err := repo.Get(ctx, db.Querier(), s.ID)
	require.NoError(t, err)
	assert.Equal(t, "tx-first", stored.TxID)
	assert.Equal(t, 1, stored.Attempts, "one transfer, one attempt")
}

func TestListsFindTheWorkThatIsLeft(t *testing.T) {
	db := pgtest.New(t)
	ctx := context.Background()
	repo := settlement.NewPostgresRepository()

	first := plan(t, seed(t, db, 4))
	require.NoError(t, repo.Create(ctx, db.Querier(), first))

	second := plan(t, seed(t, db, 5))
	require.NoError(t, repo.Create(ctx, db.Querier(), second))

	unfinished, err := repo.ListUnfinished(ctx, db.Querier(), 0)
	require.NoError(t, err)
	assert.Len(t, unfinished, 2, "a crashed saga is found here rather than lost with its process")

	// Finish one; it drops out of the queue.
	require.NoError(t, first.Submit("tx-1", testNow))
	require.NoError(t, repo.Update(ctx, db.Querier(), first, first.Version-1))
	require.NoError(t, first.ConfirmConsensus(testNow))
	require.NoError(t, repo.Update(ctx, db.Querier(), first, first.Version-1))
	require.NoError(t, first.ConfirmMirror(testNow))
	require.NoError(t, repo.Update(ctx, db.Querier(), first, first.Version-1))
	require.NoError(t, first.Account(testNow))
	require.NoError(t, repo.Update(ctx, db.Querier(), first, first.Version-1))

	unfinished, err = repo.ListUnfinished(ctx, db.Querier(), 0)
	require.NoError(t, err)
	require.Len(t, unfinished, 1)
	assert.Equal(t, second.ID, unfinished[0].ID)

	byAuction, err := repo.ListByAuction(ctx, db.Querier(), first.AuctionID)
	require.NoError(t, err)
	require.Len(t, byAuction, 1)
	assert.Equal(t, settlement.StateAccounted, byAuction[0].State)
	assert.Equal(t, "tx-1", byAuction[0].TxID)
}

// TestASettlementNeedsItsAllocation keeps a transfer from being planned against rows that
// do not exist, which is what makes the timeline and the plan agree.
func TestASettlementNeedsItsAllocation(t *testing.T) {
	db := pgtest.New(t)

	a := seed(t, db, 6)
	a.lotID = uuid.New()

	err := settlement.NewPostgresRepository().Create(context.Background(), db.Querier(), plan(t, a))
	require.ErrorIs(t, err, apperr.ErrConflict)
}
