package marketplace_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoldFridge/factorflow/internal/app/marketplace"
	"github.com/GoldFridge/factorflow/internal/auction"
	"github.com/GoldFridge/factorflow/internal/invoice"
	"github.com/GoldFridge/factorflow/internal/platform/apperr"
	"github.com/GoldFridge/factorflow/internal/platform/money"
	"github.com/GoldFridge/factorflow/internal/risk"
	"github.com/GoldFridge/factorflow/internal/testsupport/memrepo"
	"github.com/GoldFridge/factorflow/internal/tokenization"
)

var (
	testNow   = time.Date(2026, time.September, 6, 12, 0, 0, 0, time.UTC)
	testClose = testNow.Add(2 * time.Hour)
)

type fixture struct {
	service *marketplace.Service
	store   *memrepo.Store
	issuer  marketplace.Actor
	other   marketplace.Actor
	clock   time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()

	store := memrepo.New()
	f := &fixture{
		store:  store,
		issuer: marketplace.Actor{OrganizationID: uuid.MustParse("11111111-1111-4111-8111-111111111111")},
		other:  marketplace.Actor{OrganizationID: uuid.MustParse("22222222-2222-4222-8222-222222222222")},
		clock:  testNow,
	}
	f.service = marketplace.NewService(marketplace.Config{
		DB:          store,
		Invoices:    store.Invoices(),
		Assessments: store.Assessments(),
		Assets:      store.Assets(),
		Auctions:    store.Auctions(),
		Now:         func() time.Time { return f.clock },
		IDs:         uuid.New,
	})
	return f
}

// tokenizedInvoice seeds an invoice that has been assessed, approved and tokenized: the
// only state from which a receivable may be auctioned.
func (f *fixture) tokenizedInvoice(t *testing.T, issuerID uuid.UUID, number string) *invoice.Invoice {
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

	assessmentID, assetID := uuid.New(), uuid.New()
	require.NoError(t, inv.MarkUploaded(testNow))
	require.NoError(t, inv.StartAssessment(testNow))
	require.NoError(t, inv.CompleteAssessment(assessmentID, testNow))
	require.NoError(t, inv.Approve(testNow))
	require.NoError(t, inv.StartTokenization(testNow))
	require.NoError(t, inv.CompleteTokenization(assetID, testNow))
	f.store.PutInvoice(inv)

	assessment, err := risk.ModelV1().Assess(risk.AssessInput{
		ID:        assessmentID,
		InvoiceID: inv.ID,
		Face:      inv.Face,
		DaysToDue: 60,
		Features: risk.FeatureVector{
			DSONorm:             money.MustParseRate("0.40"),
			LatePaymentRate:     money.MustParseRate("0.15"),
			DisputeFlag:         money.ZeroRate(),
			DebtorConcentration: money.MustParseRate("0.38"),
			MarketVolatility:    money.MustParseRate("0.25"),
			DebtorRisk:          money.MustParseRate("0.20"),
		},
		Confidence:             money.MustParseRate("0.93"),
		ArithmeticValid:        true,
		Benchmark:              money.MustParseRate("0.0640"),
		LiquidityPremium:       money.MustParseRate("0.0150"),
		MarketSnapshotHash:     "0x3d2f1c0b9a8e7d6c5b4a39281706f5e4d3c2b1a09f8e7d6c5b4a39281706f5e4",
		ConfidentialCommitment: "0x1a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5d6e7f809",
	}, testNow)
	require.NoError(t, err)
	f.store.PutAssessment(assessment)

	asset, err := tokenization.New(tokenization.NewParams{
		ID:          assetID,
		InvoiceID:   inv.ID,
		IssuerID:    issuerID,
		Network:     tokenization.LocalNetwork,
		TokenID:     "local-token-" + number,
		Supply:      inv.Face,
		ChainStatus: tokenization.StatusIssued,
	}, testNow)
	require.NoError(t, err)
	f.store.PutAsset(asset)

	return inv
}

func (f *fixture) params(ids ...uuid.UUID) marketplace.OpenParams {
	return marketplace.OpenParams{InvoiceIDs: ids, OpensAt: testNow, ClosesAt: testClose}
}

// TestOpenAuctionBuildsLotsFromThePublishedPrice is the composition this package exists
// for: the lot's price is the one the risk model published, not one the issuer chose.
func TestOpenAuctionBuildsLotsFromThePublishedPrice(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	inv := f.tokenizedInvoice(t, f.issuer.OrganizationID, "INV-1")

	a, err := f.service.OpenAuction(context.Background(), f.issuer, f.params(inv.ID))
	require.NoError(t, err)

	assert.Equal(t, auction.StatusOpen, a.Status, "the batch is created and opened in one step")
	require.Len(t, a.Lots, 1)

	lot := a.Lots[0]
	assert.Equal(t, inv.ID, lot.InvoiceID)
	assert.Equal(t, inv.AssetID, lot.AssetID)
	assert.Equal(t, "10000.00", lot.Supply.String())
	assert.Equal(t, "9755.32", lot.ReservePrice.String(), "the reserve price comes from the assessment")
	assert.Equal(t, risk.GradeB, lot.Grade)
	assert.Equal(t, int64(60), lot.TenorDays)

	stored, ok := f.store.Invoice(inv.ID)
	require.True(t, ok)
	assert.Equal(t, invoice.StatusAuctionOpen, stored.Status, "the invoice moved with the batch")
}

// TestTenorIsMeasuredFromToday matters commercially: an invoice that waited in the queue is
// closer to maturity, and the yield an investor sees has to say so.
func TestTenorIsMeasuredFromToday(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	inv := f.tokenizedInvoice(t, f.issuer.OrganizationID, "INV-1")

	f.clock = testNow.Add(20 * 24 * time.Hour)
	a, err := f.service.OpenAuction(context.Background(), f.issuer, marketplace.OpenParams{
		InvoiceIDs: []uuid.UUID{inv.ID},
		OpensAt:    f.clock,
		ClosesAt:   f.clock.Add(time.Hour),
	})
	require.NoError(t, err)

	assert.Equal(t, int64(40), a.Lots[0].TenorDays, "40 days remain, not the original 60")

	yield, err := a.Lots[0].ImpliedYield()
	require.NoError(t, err)
	assert.Equal(t, "0.228871", yield.StringFixed(6), "the same discount over less time annualizes higher")
}

func TestOpenAuctionWithSeveralLots(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	first := f.tokenizedInvoice(t, f.issuer.OrganizationID, "INV-1")
	second := f.tokenizedInvoice(t, f.issuer.OrganizationID, "INV-2")

	a, err := f.service.OpenAuction(context.Background(), f.issuer, f.params(first.ID, second.ID))
	require.NoError(t, err)
	assert.Len(t, a.Lots, 2)

	total, err := a.TotalSupply()
	require.NoError(t, err)
	assert.Equal(t, "20000.00", total.String())
}

// TestOnlyTokenizedInvoicesCanBeAuctioned keeps an unfinanceable receivable out of the
// market: without an issued asset there is nothing to transfer at settlement.
func TestOnlyTokenizedInvoicesCanBeAuctioned(t *testing.T) {
	t.Parallel()

	f := newFixture(t)

	draft, err := invoice.New(invoice.NewParams{
		ID:        uuid.New(),
		IssuerID:  f.issuer.OrganizationID,
		DebtorRef: "ACME Logistics GmbH",
		Number:    "INV-DRAFT",
		Face:      money.MustParse("10000.00", money.USD),
		IssuedAt:  testNow,
		DueAt:     testNow.Add(60 * 24 * time.Hour),
	}, testNow)
	require.NoError(t, err)
	f.store.PutInvoice(draft)

	_, err = f.service.OpenAuction(context.Background(), f.issuer, f.params(draft.ID))
	require.ErrorIs(t, err, apperr.ErrConflict)
	assert.Equal(t, 0, f.store.AuctionCount())
}

// TestAnotherOrganizationSeesNothing repeats the disclosure rule: telling a stranger that
// an invoice exists is itself a leak.
func TestAnotherOrganizationSeesNothing(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	inv := f.tokenizedInvoice(t, f.issuer.OrganizationID, "INV-1")

	_, err := f.service.OpenAuction(context.Background(), f.other, f.params(inv.ID))
	require.ErrorIs(t, err, apperr.ErrNotFound)
	assert.NotErrorIs(t, err, apperr.ErrForbidden)
	assert.Equal(t, 0, f.store.AuctionCount())

	stored, _ := f.store.Invoice(inv.ID)
	assert.Equal(t, invoice.StatusTokenized, stored.Status, "nothing the stranger tried took effect")
}

// TestAFailedLotLeavesNothingBehind is why this composes repositories in one transaction:
// a batch that half-listed would leave invoices claiming to be in an auction that has no
// lot for them.
func TestAFailedLotLeavesNothingBehind(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	good := f.tokenizedInvoice(t, f.issuer.OrganizationID, "INV-1")
	bad := f.tokenizedInvoice(t, f.issuer.OrganizationID, "INV-2")

	// The second invoice's asset is frozen by a compliance action, so it cannot be sold.
	asset, ok := f.store.Asset(bad.AssetID)
	require.True(t, ok)
	require.NoError(t, asset.Freeze(testNow))
	f.store.PutAsset(&asset)

	_, err := f.service.OpenAuction(context.Background(), f.issuer, f.params(good.ID, bad.ID))
	require.ErrorIs(t, err, apperr.ErrConflict)

	assert.Equal(t, 0, f.store.AuctionCount(), "no batch was created")
	stored, _ := f.store.Invoice(good.ID)
	assert.Equal(t, invoice.StatusTokenized, stored.Status, "the healthy invoice did not move either")
}

func TestFrozenAssetCannotBeSold(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	inv := f.tokenizedInvoice(t, f.issuer.OrganizationID, "INV-1")

	asset, _ := f.store.Asset(inv.AssetID)
	require.NoError(t, asset.Freeze(testNow))
	f.store.PutAsset(&asset)

	_, err := f.service.OpenAuction(context.Background(), f.issuer, f.params(inv.ID))
	require.ErrorIs(t, err, apperr.ErrConflict)
	assert.Contains(t, err.Error(), "FROZEN")
}

func TestOverdueInvoiceCannotBeAuctioned(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	inv := f.tokenizedInvoice(t, f.issuer.OrganizationID, "INV-1")

	f.clock = testNow.Add(70 * 24 * time.Hour)
	_, err := f.service.OpenAuction(context.Background(), f.issuer, marketplace.OpenParams{
		InvoiceIDs: []uuid.UUID{inv.ID},
		OpensAt:    f.clock,
		ClosesAt:   f.clock.Add(time.Hour),
	})
	require.ErrorIs(t, err, apperr.ErrConflict)
	assert.Contains(t, err.Error(), "already due")
}

func TestOpenAuctionValidation(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	inv := f.tokenizedInvoice(t, f.issuer.OrganizationID, "INV-1")
	ctx := context.Background()

	_, err := f.service.OpenAuction(ctx, marketplace.Actor{}, f.params(inv.ID))
	require.ErrorIs(t, err, apperr.ErrForbidden)

	_, err = f.service.OpenAuction(ctx, f.issuer, f.params())
	require.ErrorIs(t, err, apperr.ErrValidation)

	_, err = f.service.OpenAuction(ctx, f.issuer, f.params(inv.ID, inv.ID))
	require.ErrorIs(t, err, apperr.ErrValidation, "the same invoice cannot be listed twice")

	tooMany := make([]uuid.UUID, marketplace.MaxLotsPerAuction+1)
	for i := range tooMany {
		tooMany[i] = uuid.New()
	}
	_, err = f.service.OpenAuction(ctx, f.issuer, f.params(tooMany...))
	require.ErrorIs(t, err, apperr.ErrValidation)

	_, err = f.service.OpenAuction(ctx, f.issuer, marketplace.OpenParams{
		InvoiceIDs: []uuid.UUID{inv.ID}, OpensAt: testClose, ClosesAt: testNow,
	})
	require.ErrorIs(t, err, apperr.ErrValidation, "a window that closes before it opens is refused")
}

func TestUnknownInvoiceIsReported(t *testing.T) {
	t.Parallel()

	f := newFixture(t)

	_, err := f.service.OpenAuction(context.Background(), f.issuer, f.params(uuid.New()))
	require.ErrorIs(t, err, apperr.ErrNotFound)
}

func TestFailedCommitLeavesNoAuction(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	inv := f.tokenizedInvoice(t, f.issuer.OrganizationID, "INV-1")
	f.store.FailCommit = errors.New("commit failed")

	_, err := f.service.OpenAuction(context.Background(), f.issuer, f.params(inv.ID))
	require.Error(t, err)
	f.store.FailCommit = nil

	assert.Equal(t, 0, f.store.AuctionCount())
	stored, _ := f.store.Invoice(inv.ID)
	assert.Equal(t, invoice.StatusTokenized, stored.Status)
}

// TestAnAssetIsListedOnce mirrors the database constraint: the same receivable cannot be
// sold in two parallel batches.
func TestAnAssetIsListedOnce(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	inv := f.tokenizedInvoice(t, f.issuer.OrganizationID, "INV-1")

	_, err := f.service.OpenAuction(context.Background(), f.issuer, f.params(inv.ID))
	require.NoError(t, err)

	// Force the invoice back so only the asset constraint can refuse the second listing.
	stored, _ := f.store.Invoice(inv.ID)
	require.NoError(t, stored.CancelAuction("re-listing", testNow))
	f.store.PutInvoice(&stored)

	_, err = f.service.OpenAuction(context.Background(), f.issuer, f.params(inv.ID))
	require.ErrorIs(t, err, apperr.ErrConflict)
	assert.Equal(t, 1, f.store.AuctionCount())
	assert.Contains(t, fmt.Sprint(err), "already in an auction")
}

// bid places an investor's bid straight through the repository: this package's tests are
// about clearing, and the bidding rules are the auction module's own.
func (f *fixture) bid(t *testing.T, a *auction.Auction, investor uuid.UUID, minYield, maxGrade string, created time.Time) *auction.Bid {
	t.Helper()

	grade, err := risk.ParseGrade(maxGrade)
	require.NoError(t, err)

	b, err := auction.NewBid(auction.NewBidParams{
		ID:           uuid.New(),
		AuctionID:    a.ID,
		InvestorID:   investor,
		Budget:       money.MustParse("50000.00", money.USD),
		MinYield:     money.MustParseRate(minYield),
		MaxGrade:     grade,
		MaxTenorDays: 120,
		MinimumLot:   money.Zero(money.USD),
	}, created)
	require.NoError(t, err)
	require.NoError(t, f.store.Auctions().CreateBid(context.Background(), f.store.Querier(), b))
	return b
}

// TestClearMovesTheInvoicesToo is why clearing lives here: an investor holding an
// allocation whose invoice still claims to be on sale would be a contradiction nobody could
// resolve later.
func TestClearMovesTheInvoicesToo(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	inv := f.tokenizedInvoice(t, f.issuer.OrganizationID, "INV-1")

	a, err := f.service.OpenAuction(context.Background(), f.issuer, f.params(inv.ID))
	require.NoError(t, err)

	winner := f.bid(t, a, uuid.New(), "0.05", "E", testNow)
	loser := f.bid(t, a, uuid.New(), "0.90", "E", testNow.Add(time.Minute))

	f.clock = testClose
	solution, err := f.service.ClearAuction(context.Background(), f.issuer, a.ID)
	require.NoError(t, err)

	require.Len(t, solution.Allocations, 1)
	assert.Equal(t, winner.ID, solution.Allocations[0].BidID)
	assert.True(t, solution.Verified)

	require.Len(t, solution.Rejections, 1)
	assert.Equal(t, loser.ID, solution.Rejections[0].BidID)
	assert.Equal(t, auction.ConstraintYield, solution.Rejections[0].Constraint,
		"the loser is told which of its own limits refused the lot")

	cleared, ok := f.store.Auction(a.ID)
	require.True(t, ok)
	assert.Equal(t, auction.StatusCleared, cleared.Status)
	assert.Equal(t, solution.CertificateHash, cleared.CertificateHash)

	stored, ok := f.store.Invoice(inv.ID)
	require.True(t, ok)
	assert.Equal(t, invoice.StatusAllocated, stored.Status, "the financed invoice moved with the clearing")
}

// TestUnsoldLotReturnsToItsIssuer keeps an unsold receivable usable: it goes back to
// TOKENIZED so it can be listed again rather than being stuck in a finished auction.
func TestUnsoldLotReturnsToItsIssuer(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	inv := f.tokenizedInvoice(t, f.issuer.OrganizationID, "INV-1")

	a, err := f.service.OpenAuction(context.Background(), f.issuer, f.params(inv.ID))
	require.NoError(t, err)

	// The only bid demands a yield this lot cannot offer.
	f.bid(t, a, uuid.New(), "0.90", "E", testNow)

	f.clock = testClose
	solution, err := f.service.ClearAuction(context.Background(), f.issuer, a.ID)
	require.NoError(t, err)
	assert.Empty(t, solution.Allocations, "no feasible match still clears")

	cleared, _ := f.store.Auction(a.ID)
	assert.Equal(t, auction.StatusCleared, cleared.Status)

	stored, _ := f.store.Invoice(inv.ID)
	assert.Equal(t, invoice.StatusTokenized, stored.Status, "the issuer can list it again")
}

func TestClearWaitsForTheClose(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	inv := f.tokenizedInvoice(t, f.issuer.OrganizationID, "INV-1")

	a, err := f.service.OpenAuction(context.Background(), f.issuer, f.params(inv.ID))
	require.NoError(t, err)

	_, err = f.service.ClearAuction(context.Background(), f.issuer, a.ID)
	require.ErrorIs(t, err, apperr.ErrConflict)

	stored, _ := f.store.Auction(a.ID)
	assert.Equal(t, auction.StatusOpen, stored.Status, "a refused clearing does not move the auction")
}

func TestClearIsForTheIssuerOnly(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	inv := f.tokenizedInvoice(t, f.issuer.OrganizationID, "INV-1")

	a, err := f.service.OpenAuction(context.Background(), f.issuer, f.params(inv.ID))
	require.NoError(t, err)

	f.clock = testClose
	_, err = f.service.ClearAuction(context.Background(), f.other, a.ID)
	require.ErrorIs(t, err, apperr.ErrForbidden)

	operator := marketplace.Actor{OrganizationID: uuid.New(), Operator: true}
	_, err = f.service.ClearAuction(context.Background(), operator, a.ID)
	require.NoError(t, err, "an operator may clear a stuck batch")
}

// TestFailedClearingRollsBackWhole is the safety property of doing it in one transaction:
// a failure leaves the auction, the bids and the invoices exactly as they were.
func TestFailedClearingRollsBackWhole(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	inv := f.tokenizedInvoice(t, f.issuer.OrganizationID, "INV-1")

	a, err := f.service.OpenAuction(context.Background(), f.issuer, f.params(inv.ID))
	require.NoError(t, err)
	f.bid(t, a, uuid.New(), "0.05", "E", testNow)

	f.clock = testClose
	f.store.FailCommit = errors.New("storage went away mid-clearing")

	_, err = f.service.ClearAuction(context.Background(), f.issuer, a.ID)
	require.Error(t, err)
	f.store.FailCommit = nil

	stored, _ := f.store.Auction(a.ID)
	assert.Equal(t, auction.StatusOpen, stored.Status)
	assert.Empty(t, stored.CertificateHash, "nothing was certified")

	storedInvoice, _ := f.store.Invoice(inv.ID)
	assert.Equal(t, invoice.StatusAuctionOpen, storedInvoice.Status, "the invoice did not move either")

	// And the retry works once the cause is gone.
	solution, err := f.service.ClearAuction(context.Background(), f.issuer, a.ID)
	require.NoError(t, err)
	assert.Len(t, solution.Allocations, 1)
}

func TestClearUnknownAuction(t *testing.T) {
	t.Parallel()

	f := newFixture(t)

	_, err := f.service.ClearAuction(context.Background(), f.issuer, uuid.New())
	require.ErrorIs(t, err, apperr.ErrNotFound)
}
