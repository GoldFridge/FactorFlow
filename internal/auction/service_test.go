package auction_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoldFridge/factorflow/internal/auction"
	"github.com/GoldFridge/factorflow/internal/platform/apperr"
	"github.com/GoldFridge/factorflow/internal/platform/money"
	"github.com/GoldFridge/factorflow/internal/risk"
)

// serviceFixture wires the service over the in-memory repository with a controllable clock.
type serviceFixture struct {
	service  *auction.Service
	repo     *memRepository
	db       *fakeDB
	issuer   auction.Actor
	investor auction.Actor
	other    auction.Actor
	clock    time.Time
}

func newServiceFixture(t *testing.T) *serviceFixture {
	t.Helper()

	repo := newMemRepository()
	f := &serviceFixture{
		repo:     repo,
		db:       &fakeDB{repo: repo},
		issuer:   auction.Actor{OrganizationID: issuerA(), Eligible: true},
		investor: auction.Actor{OrganizationID: seqUUID(0x70, 1), Eligible: true},
		other:    auction.Actor{OrganizationID: seqUUID(0x70, 2), Eligible: true},
		clock:    testNow,
	}
	f.service = auction.NewService(f.db, repo, auction.NewSolver(), func() time.Time { return f.clock }, uuid.New)
	return f
}

func (f *serviceFixture) createParams(lots ...auction.Lot) auction.CreateParams {
	if len(lots) == 0 {
		lots = []auction.Lot{lot(1, "ACME", issuerA(), "10000.00", "9755.32", risk.GradeB, 60)}
	}
	return auction.CreateParams{Lots: lots, OpensAt: testOpens, ClosesAt: testClose}
}

func (f *serviceFixture) bidParams() auction.BidParams {
	return auction.BidParams{
		Budget:       money.MustParse("50000.00", money.USD),
		MinYield:     money.MustParseRate("0.08"),
		MaxGrade:     risk.GradeC,
		MaxTenorDays: 120,
		MinimumLot:   money.Zero(money.USD),
	}
}

// openAuction returns an auction that is accepting bids.
func (f *serviceFixture) openAuction(t *testing.T, lots ...auction.Lot) *auction.Auction {
	t.Helper()

	ctx := context.Background()
	a, err := f.service.Create(ctx, f.issuer, f.createParams(lots...))
	require.NoError(t, err)

	a, err = f.service.Open(ctx, f.issuer, a.ID)
	require.NoError(t, err)
	return a
}

func TestServiceCreate(t *testing.T) {
	t.Parallel()

	f := newServiceFixture(t)
	a, err := f.service.Create(context.Background(), f.issuer, f.createParams())
	require.NoError(t, err)

	assert.Equal(t, auction.StatusDraft, a.Status)
	assert.Equal(t, f.issuer.OrganizationID, a.IssuerID, "the issuer is the caller, never a body field")
	assert.Len(t, a.Lots, 1)
}

// TestCreateRefusesAnotherIssuersLot is the rule that stops an issuer listing a receivable
// it does not own and collecting the proceeds.
func TestCreateRefusesAnotherIssuersLot(t *testing.T) {
	t.Parallel()

	f := newServiceFixture(t)
	foreign := lot(2, "BOLT", issuerB(), "10000.00", "9700.00", risk.GradeB, 60)

	_, err := f.service.Create(context.Background(), f.issuer, f.createParams(foreign))
	require.ErrorIs(t, err, apperr.ErrForbidden)
}

func TestCreateNeedsAnOrganization(t *testing.T) {
	t.Parallel()

	f := newServiceFixture(t)

	_, err := f.service.Create(context.Background(), auction.Actor{}, f.createParams())
	require.ErrorIs(t, err, apperr.ErrForbidden)
}

func TestOnlyTheIssuerMovesTheAuction(t *testing.T) {
	t.Parallel()

	f := newServiceFixture(t)
	ctx := context.Background()

	a, err := f.service.Create(ctx, f.issuer, f.createParams())
	require.NoError(t, err)

	_, err = f.service.Open(ctx, f.investor, a.ID)
	require.ErrorIs(t, err, apperr.ErrForbidden)

	stored, err := f.service.Get(ctx, f.issuer, a.ID)
	require.NoError(t, err)
	assert.Equal(t, auction.StatusDraft, stored.Status, "nothing the stranger tried took effect")

	operator := auction.Actor{OrganizationID: uuid.New(), Operator: true}
	_, err = f.service.Open(ctx, operator, a.ID)
	require.NoError(t, err, "an operator may move a stuck auction")
}

func TestPlaceBid(t *testing.T) {
	t.Parallel()

	f := newServiceFixture(t)
	a := f.openAuction(t)

	bid, err := f.service.PlaceBid(context.Background(), f.investor, a.ID, f.bidParams())
	require.NoError(t, err)

	assert.Equal(t, auction.BidStatusActive, bid.Status)
	assert.Equal(t, f.investor.OrganizationID, bid.InvestorID)
	assert.Equal(t, a.ID, bid.AuctionID)
}

// TestIssuerCannotBidOnItsOwnAuction stops an issuer buying its own receivable, which is
// either a mistake or an attempt to set the clearing price.
func TestIssuerCannotBidOnItsOwnAuction(t *testing.T) {
	t.Parallel()

	f := newServiceFixture(t)
	a := f.openAuction(t)

	_, err := f.service.PlaceBid(context.Background(), f.issuer, a.ID, f.bidParams())
	require.ErrorIs(t, err, apperr.ErrForbidden)
}

func TestIneligibleInvestorCannotBid(t *testing.T) {
	t.Parallel()

	f := newServiceFixture(t)
	a := f.openAuction(t)

	pending := auction.Actor{OrganizationID: seqUUID(0x70, 9), Eligible: false}
	_, err := f.service.PlaceBid(context.Background(), pending, a.ID, f.bidParams())
	require.ErrorIs(t, err, apperr.ErrForbidden)
}

func TestBidsAreRefusedOutsideTheBiddingWindow(t *testing.T) {
	t.Parallel()

	f := newServiceFixture(t)
	ctx := context.Background()

	draft, err := f.service.Create(ctx, f.issuer, f.createParams())
	require.NoError(t, err)

	_, err = f.service.PlaceBid(ctx, f.investor, draft.ID, f.bidParams())
	require.ErrorIs(t, err, apperr.ErrConflict, "a draft takes no bids")

	a := f.openAuction(t, lot(3, "CIRRUS", issuerA(), "8000.00", "7800.00", risk.GradeB, 45))
	f.clock = testClose
	_, err = f.service.PlaceBid(ctx, f.investor, a.ID, f.bidParams())
	require.ErrorIs(t, err, apperr.ErrConflict, "bidding stops at the closing time")
}

func TestBidCurrencyMustMatchTheAuction(t *testing.T) {
	t.Parallel()

	f := newServiceFixture(t)
	a := f.openAuction(t)

	params := f.bidParams()
	params.Budget = money.MustParse("50000.00", money.EUR)
	params.MinimumLot = money.Zero(money.EUR)

	_, err := f.service.PlaceBid(context.Background(), f.investor, a.ID, params)
	require.ErrorIs(t, err, apperr.ErrValidation)
}

func TestCancelBidThroughTheService(t *testing.T) {
	t.Parallel()

	f := newServiceFixture(t)
	ctx := context.Background()
	a := f.openAuction(t)

	bid, err := f.service.PlaceBid(ctx, f.investor, a.ID, f.bidParams())
	require.NoError(t, err)

	// Someone else's bid is invisible, not merely forbidden.
	_, err = f.service.CancelBid(ctx, f.other, bid.ID)
	require.ErrorIs(t, err, apperr.ErrNotFound)

	cancelled, err := f.service.CancelBid(ctx, f.investor, bid.ID)
	require.NoError(t, err)
	assert.Equal(t, auction.BidStatusCancelled, cancelled.Status)

	_, err = f.service.CancelBid(ctx, f.investor, bid.ID)
	require.ErrorIs(t, err, apperr.ErrConflict, "a withdrawn bid cannot be withdrawn twice")
}

// TestBidVisibility keeps one investor from reading another's risk appetite, while the
// issuer running the batch sees everything it is about to clear.
func TestBidVisibility(t *testing.T) {
	t.Parallel()

	f := newServiceFixture(t)
	ctx := context.Background()
	a := f.openAuction(t)

	mine, err := f.service.PlaceBid(ctx, f.investor, a.ID, f.bidParams())
	require.NoError(t, err)
	_, err = f.service.PlaceBid(ctx, f.other, a.ID, f.bidParams())
	require.NoError(t, err)

	visible, err := f.service.Bids(ctx, f.investor, a.ID)
	require.NoError(t, err)
	require.Len(t, visible, 1)
	assert.Equal(t, mine.ID, visible[0].ID)

	all, err := f.service.Bids(ctx, f.issuer, a.ID)
	require.NoError(t, err)
	assert.Len(t, all, 2)

	operatorView, err := f.service.Bids(ctx, auction.Actor{OrganizationID: uuid.New(), Operator: true}, a.ID)
	require.NoError(t, err)
	assert.Len(t, operatorView, 2)
}

func TestListAndGetNeedAuthentication(t *testing.T) {
	t.Parallel()

	f := newServiceFixture(t)
	ctx := context.Background()
	a := f.openAuction(t)

	_, err := f.service.List(ctx, auction.Actor{}, "", 10)
	require.ErrorIs(t, err, apperr.ErrForbidden)

	_, err = f.service.Get(ctx, auction.Actor{}, a.ID)
	require.ErrorIs(t, err, apperr.ErrForbidden)

	_, err = f.service.List(ctx, f.investor, auction.Status("SOLD"), 10)
	require.ErrorIs(t, err, apperr.ErrValidation)

	open, err := f.service.List(ctx, f.investor, auction.StatusOpen, 10)
	require.NoError(t, err)
	assert.Len(t, open, 1, "the marketplace is visible to any authenticated caller")
}

func TestUnknownAuctionAndBid(t *testing.T) {
	t.Parallel()

	f := newServiceFixture(t)
	ctx := context.Background()

	_, err := f.service.Get(ctx, f.investor, uuid.New())
	require.ErrorIs(t, err, apperr.ErrNotFound)

	_, err = f.service.CancelBid(ctx, f.investor, uuid.New())
	require.ErrorIs(t, err, apperr.ErrNotFound)

	_, err = f.service.Solution(ctx, f.investor, uuid.New())
	require.ErrorIs(t, err, apperr.ErrNotFound)
}
