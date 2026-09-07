package auction_test

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
)

func TestMain(m *testing.M) {
	code := m.Run()
	pgtest.Shutdown()
	os.Exit(code)
}

// seedIssuer inserts an organization, since auctions, lots and bids all reference one.
func seedIssuer(t *testing.T, db *postgres.DB, seq int) uuid.UUID {
	t.Helper()

	org, err := organization.New(organization.NewParams{
		ID:     uuid.New(),
		Type:   organization.TypeIssuer,
		Name:   fmt.Sprintf("Organization %d", seq),
		Wallet: fmt.Sprintf("0x%040x", seq),
	}, testNow)
	require.NoError(t, err)
	require.NoError(t, org.Approve(testNow))
	require.NoError(t, organization.NewPostgresRepository().Create(context.Background(), db.Querier(), org))
	return org.ID
}

// seedInvoice inserts the invoice a lot points at, so the lot's foreign key resolves.
func seedInvoice(t *testing.T, db *postgres.DB, issuerID uuid.UUID, number string) uuid.UUID {
	t.Helper()

	inv, err := invoice.New(invoice.NewParams{
		ID:        uuid.New(),
		IssuerID:  issuerID,
		DebtorRef: "ACME Logistics GmbH",
		Number:    number,
		Face:      money.MustParse("10000.00", money.USD),
		IssuedAt:  testNow,
		DueAt:     testNow.Add(60 * 24 * time.Hour),
	}, testNow)
	require.NoError(t, err)
	require.NoError(t, invoice.NewPostgresRepository().Create(context.Background(), db.Querier(), inv))
	return inv.ID
}

// storedAuction builds an auction whose lot references real rows.
func storedAuction(t *testing.T, db *postgres.DB, issuerID uuid.UUID, seq int) *auction.Auction {
	t.Helper()

	invoiceID := seedInvoice(t, db, issuerID, fmt.Sprintf("INV-2026-%04d", seq))
	l := auction.Lot{
		ID:           uuid.New(),
		InvoiceID:    invoiceID,
		AssetID:      uuid.New(),
		IssuerID:     issuerID,
		DebtorRef:    "ACME Logistics GmbH",
		Supply:       money.MustParse("10000.00", money.USD),
		ReservePrice: money.MustParse("9755.32", money.USD),
		Grade:        risk.GradeB,
		TenorDays:    60,
	}

	a, err := auction.NewAuction(auction.NewAuctionParams{
		ID:       uuid.New(),
		IssuerID: issuerID,
		Lots:     []auction.Lot{l},
		OpensAt:  testOpens,
		ClosesAt: testClose,
	}, testNow)
	require.NoError(t, err)
	return a
}

func TestAuctionRoundTrip(t *testing.T) {
	db := pgtest.New(t)
	ctx := context.Background()
	repo := auction.NewPostgresRepository()

	issuerID := seedIssuer(t, db, 1)
	a := storedAuction(t, db, issuerID, 1)
	require.NoError(t, repo.CreateAuction(ctx, db.Querier(), a))

	got, err := repo.GetAuction(ctx, db.Querier(), a.ID)
	require.NoError(t, err)

	assert.Equal(t, a.ID, got.ID)
	assert.Equal(t, auction.StatusDraft, got.Status)
	assert.Equal(t, money.USD, got.Currency())
	require.Len(t, got.Lots, 1)

	stored := got.Lots[0]
	assert.Equal(t, "10000.00", stored.Supply.String(), "money survives as integer minor units")
	assert.Equal(t, "9755.32", stored.ReservePrice.String())
	assert.Equal(t, risk.GradeB, stored.Grade)
	assert.Equal(t, int64(60), stored.TenorDays)

	yield, err := stored.ImpliedYield()
	require.NoError(t, err)
	assert.Equal(t, "0.152580", yield.StringFixed(6), "a lot read back prices exactly as it was written")
}

func TestGetMissingAuction(t *testing.T) {
	db := pgtest.New(t)

	_, err := auction.NewPostgresRepository().GetAuction(context.Background(), db.Querier(), uuid.New())
	require.ErrorIs(t, err, apperr.ErrNotFound)
}

// TestAnAssetIsAuctionedOnce covers the constraint that makes an exposure limit meaningful:
// the same receivable cannot be sold in two parallel batches.
func TestAnAssetIsAuctionedOnce(t *testing.T) {
	db := pgtest.New(t)
	ctx := context.Background()
	repo := auction.NewPostgresRepository()

	issuerID := seedIssuer(t, db, 2)
	first := storedAuction(t, db, issuerID, 2)
	require.NoError(t, repo.CreateAuction(ctx, db.Querier(), first))

	second := storedAuction(t, db, issuerID, 3)
	second.Lots[0].AssetID = first.Lots[0].AssetID

	require.ErrorIs(t, repo.CreateAuction(ctx, db.Querier(), second), apperr.ErrConflict)
}

func TestAuctionOptimisticConcurrency(t *testing.T) {
	db := pgtest.New(t)
	ctx := context.Background()
	repo := auction.NewPostgresRepository()

	issuerID := seedIssuer(t, db, 4)
	a := storedAuction(t, db, issuerID, 4)
	require.NoError(t, repo.CreateAuction(ctx, db.Querier(), a))

	writerA, err := repo.GetAuction(ctx, db.Querier(), a.ID)
	require.NoError(t, err)
	writerB, err := repo.GetAuction(ctx, db.Querier(), a.ID)
	require.NoError(t, err)

	require.NoError(t, writerA.Open(testOpens))
	require.NoError(t, repo.UpdateAuction(ctx, db.Querier(), writerA, 1))

	require.NoError(t, writerB.Cancel("stale decision", testOpens))
	require.ErrorIs(t, repo.UpdateAuction(ctx, db.Querier(), writerB, 1), apperr.ErrConflict)

	stored, err := repo.GetAuction(ctx, db.Querier(), a.ID)
	require.NoError(t, err)
	assert.Equal(t, auction.StatusOpen, stored.Status, "the first writer's decision stands")
}

func TestBidRoundTrip(t *testing.T) {
	db := pgtest.New(t)
	ctx := context.Background()
	repo := auction.NewPostgresRepository()

	issuerID := seedIssuer(t, db, 5)
	investorID := seedIssuer(t, db, 6)
	a := storedAuction(t, db, issuerID, 5)
	require.NoError(t, repo.CreateAuction(ctx, db.Querier(), a))

	bid, err := auction.NewBid(auction.NewBidParams{
		ID:             uuid.New(),
		AuctionID:      a.ID,
		InvestorID:     investorID,
		Budget:         money.MustParse("50000.00", money.USD),
		MinYield:       money.MustParseRate("0.08"),
		MaxGrade:       risk.GradeC,
		MaxTenorDays:   120,
		MinimumLot:     money.MustParse("1000.00", money.USD),
		MaxIssuerShare: money.MustParseRate("0.40"),
		MaxDebtorShare: money.MustParseRate("0.25"),
		MaxGradeShare:  map[risk.Grade]money.Rate{risk.GradeC: money.MustParseRate("0.30")},
	}, testNow)
	require.NoError(t, err)
	require.NoError(t, repo.CreateBid(ctx, db.Querier(), bid))

	got, err := repo.GetBid(ctx, db.Querier(), bid.ID)
	require.NoError(t, err)

	assert.Equal(t, "50000.00", got.Budget.String())
	assert.Equal(t, "0.080000", got.MinYield.StringFixed(6))
	assert.Equal(t, risk.GradeC, got.MaxGrade)
	assert.Equal(t, "1000.00", got.MinimumLot.String())
	assert.Equal(t, auction.BidStatusActive, got.Status)

	// The per-grade limits are the part most easily lost in storage: they are a map.
	require.Len(t, got.MaxGradeShare, 1)
	assert.Equal(t, "0.300000", got.MaxGradeShare[risk.GradeC].StringFixed(6))

	issuerLimit, err := got.IssuerLimit()
	require.NoError(t, err)
	assert.Equal(t, "20000.00", issuerLimit.String(), "limits recompute from what was stored")
}

// TestBidsComeBackInSolverOrder matters for determinism: the solver breaks ties by creation
// time and id, so the database has to hand them over the same way.
func TestBidsComeBackInSolverOrder(t *testing.T) {
	db := pgtest.New(t)
	ctx := context.Background()
	repo := auction.NewPostgresRepository()

	issuerID := seedIssuer(t, db, 7)
	a := storedAuction(t, db, issuerID, 7)
	require.NoError(t, repo.CreateAuction(ctx, db.Querier(), a))

	// Insert out of order; the query must still return them in order.
	for _, offset := range []int{3, 1, 2} {
		investorID := seedIssuer(t, db, 100+offset)
		bid, err := auction.NewBid(auction.NewBidParams{
			ID:           uuid.New(),
			AuctionID:    a.ID,
			InvestorID:   investorID,
			Budget:       money.MustParse("10000.00", money.USD),
			MinYield:     money.MustParseRate("0.05"),
			MaxGrade:     risk.GradeC,
			MaxTenorDays: 120,
			MinimumLot:   money.Zero(money.USD),
		}, testNow.Add(time.Duration(offset)*time.Minute))
		require.NoError(t, err)
		require.NoError(t, repo.CreateBid(ctx, db.Querier(), bid))
	}

	bids, err := repo.ListBids(ctx, db.Querier(), a.ID)
	require.NoError(t, err)
	require.Len(t, bids, 3)

	for i := 1; i < len(bids); i++ {
		assert.Truef(t, bids[i-1].CreatedAt.Before(bids[i].CreatedAt),
			"bid %d was returned before an older one", i)
	}
}

// TestSolutionRoundTrip stores a real clearing and reads it back: the certificate is only
// evidence if it survives storage unchanged.
func TestSolutionRoundTrip(t *testing.T) {
	db := pgtest.New(t)
	ctx := context.Background()
	repo := auction.NewPostgresRepository()

	issuerID := seedIssuer(t, db, 8)
	investorID := seedIssuer(t, db, 9)
	a := storedAuction(t, db, issuerID, 8)
	require.NoError(t, repo.CreateAuction(ctx, db.Querier(), a))

	winner, err := auction.NewBid(auction.NewBidParams{
		ID: uuid.New(), AuctionID: a.ID, InvestorID: investorID,
		Budget: money.MustParse("50000.00", money.USD), MinYield: money.MustParseRate("0.08"),
		MaxGrade: risk.GradeC, MaxTenorDays: 120, MinimumLot: money.Zero(money.USD),
	}, testNow)
	require.NoError(t, err)
	require.NoError(t, repo.CreateBid(ctx, db.Querier(), winner))

	loserID := seedIssuer(t, db, 10)
	loser, err := auction.NewBid(auction.NewBidParams{
		ID: uuid.New(), AuctionID: a.ID, InvestorID: loserID,
		Budget: money.MustParse("50000.00", money.USD), MinYield: money.MustParseRate("0.90"),
		MaxGrade: risk.GradeC, MaxTenorDays: 120, MinimumLot: money.Zero(money.USD),
	}, testNow.Add(time.Minute))
	require.NoError(t, err)
	require.NoError(t, repo.CreateBid(ctx, db.Querier(), loser))

	stored, err := repo.GetAuction(ctx, db.Querier(), a.ID)
	require.NoError(t, err)
	bids, err := repo.ListBids(ctx, db.Querier(), a.ID)
	require.NoError(t, err)

	solution, err := auction.NewSolver().Clear(stored, bids, testClose)
	require.NoError(t, err)
	require.Len(t, solution.Allocations, 1)

	require.NoError(t, repo.SaveSolution(ctx, db.Querier(), solution, testClose))

	got, err := repo.GetSolution(ctx, db.Querier(), a.ID)
	require.NoError(t, err)

	assert.Equal(t, solution.CertificateHash, got.CertificateHash)
	assert.Equal(t, solution.SolverVersion, got.SolverVersion)
	assert.Equal(t, solution.Objective, got.Objective)
	assert.Equal(t, solution.TotalNotional.String(), got.TotalNotional.String())
	assert.Equal(t, solution.TotalCash.String(), got.TotalCash.String())
	assert.True(t, got.Verified)

	require.Len(t, got.Allocations, 1)
	assert.Equal(t, winner.ID, got.Allocations[0].BidID)
	assert.Equal(t, solution.Allocations[0].Notional.String(), got.Allocations[0].Notional.String())
	assert.Equal(t, solution.Allocations[0].Price.String(), got.Allocations[0].Price.String())
	assert.Equal(t, stored.Lots[0].InvoiceID, got.Allocations[0].InvoiceID)

	require.Len(t, got.Rejections, 1)
	assert.Equal(t, loser.ID, got.Rejections[0].BidID)
	assert.Equal(t, auction.ConstraintYield, got.Rejections[0].Constraint)
}

func TestListAuctions(t *testing.T) {
	db := pgtest.New(t)
	ctx := context.Background()
	repo := auction.NewPostgresRepository()

	issuerID := seedIssuer(t, db, 11)

	open := storedAuction(t, db, issuerID, 11)
	require.NoError(t, repo.CreateAuction(ctx, db.Querier(), open))
	require.NoError(t, open.Open(testOpens))
	require.NoError(t, repo.UpdateAuction(ctx, db.Querier(), open, 1))

	draft := storedAuction(t, db, issuerID, 12)
	require.NoError(t, repo.CreateAuction(ctx, db.Querier(), draft))

	all, err := repo.ListAuctions(ctx, db.Querier(), "", 10)
	require.NoError(t, err)
	assert.Len(t, all, 2)
	for _, a := range all {
		assert.Lenf(t, a.Lots, 1, "auction %s came back without its lots", a.ID)
	}

	onlyOpen, err := repo.ListAuctions(ctx, db.Querier(), auction.StatusOpen, 10)
	require.NoError(t, err)
	require.Len(t, onlyOpen, 1)
	assert.Equal(t, open.ID, onlyOpen[0].ID)
}

func TestAuctionNeedsAKnownIssuer(t *testing.T) {
	db := pgtest.New(t)

	issuerID := seedIssuer(t, db, 13)
	a := storedAuction(t, db, issuerID, 13)
	a.IssuerID = uuid.New()

	err := auction.NewPostgresRepository().CreateAuction(context.Background(), db.Querier(), a)
	require.ErrorIs(t, err, apperr.ErrConflict)
}
