package auction_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/skimer2king/factorflow/internal/auction"
	"github.com/skimer2king/factorflow/internal/platform/apperr"
	"github.com/skimer2king/factorflow/internal/platform/money"
	"github.com/skimer2king/factorflow/internal/risk"
)

var (
	testNow   = time.Date(2026, time.September, 5, 12, 0, 0, 0, time.UTC)
	testOpens = testNow
	testClose = testNow.Add(2 * time.Hour)
)

// testLot is a 60-day B-grade receivable of 10 000 face priced at 9 755.32, the reserve
// price the risk model publishes for the specification's reference invoice.
func testLot() auction.Lot {
	return auction.Lot{
		ID:           uuid.New(),
		InvoiceID:    uuid.New(),
		AssetID:      uuid.New(),
		IssuerID:     uuid.New(),
		DebtorRef:    "ACME Logistics GmbH",
		Supply:       money.MustParse("10000.00", money.USD),
		ReservePrice: money.MustParse("9755.32", money.USD),
		Grade:        risk.GradeB,
		TenorDays:    60,
	}
}

func newAuction(t *testing.T, lots ...auction.Lot) *auction.Auction {
	t.Helper()

	if len(lots) == 0 {
		lots = []auction.Lot{testLot()}
	}
	a, err := auction.NewAuction(auction.NewAuctionParams{
		ID:       uuid.New(),
		IssuerID: lots[0].IssuerID,
		Lots:     lots,
		OpensAt:  testOpens,
		ClosesAt: testClose,
	}, testNow)
	require.NoError(t, err)
	return a
}

func TestNewAuction(t *testing.T) {
	t.Parallel()

	lot := testLot()
	a := newAuction(t, lot)

	assert.Equal(t, auction.StatusDraft, a.Status)
	assert.Equal(t, int64(1), a.Version)
	assert.Equal(t, money.USD, a.Currency())

	total, err := a.TotalSupply()
	require.NoError(t, err)
	assert.Equal(t, "10000.00", total.String())

	got, ok := a.Lot(lot.ID)
	require.True(t, ok)
	assert.Equal(t, lot.AssetID, got.AssetID)

	_, ok = a.Lot(uuid.New())
	assert.False(t, ok)
}

func TestNewAuctionRejectsBrokenBatches(t *testing.T) {
	t.Parallel()

	sameAsset := testLot()
	duplicate := testLot()
	duplicate.AssetID = sameAsset.AssetID

	eur := testLot()
	eur.Supply = money.MustParse("10000.00", money.EUR)
	eur.ReservePrice = money.MustParse("9755.32", money.EUR)

	tests := []struct {
		name      string
		params    auction.NewAuctionParams
		wantField string
	}{
		{
			name:      "no lots",
			params:    auction.NewAuctionParams{ID: uuid.New(), IssuerID: uuid.New(), OpensAt: testOpens, ClosesAt: testClose},
			wantField: "lots",
		},
		{
			name: "nil id",
			params: auction.NewAuctionParams{
				IssuerID: uuid.New(), Lots: []auction.Lot{testLot()}, OpensAt: testOpens, ClosesAt: testClose,
			},
			wantField: "id",
		},
		{
			name: "nil issuer",
			params: auction.NewAuctionParams{
				ID: uuid.New(), Lots: []auction.Lot{testLot()}, OpensAt: testOpens, ClosesAt: testClose,
			},
			wantField: "issuer_id",
		},
		{
			name: "closes before it opens",
			params: auction.NewAuctionParams{
				ID: uuid.New(), IssuerID: uuid.New(), Lots: []auction.Lot{testLot()},
				OpensAt: testClose, ClosesAt: testOpens,
			},
			wantField: "closes_at",
		},
		{
			name: "the same asset twice",
			params: auction.NewAuctionParams{
				ID: uuid.New(), IssuerID: uuid.New(), Lots: []auction.Lot{sameAsset, duplicate},
				OpensAt: testOpens, ClosesAt: testClose,
			},
			wantField: "lots",
		},
		{
			name: "mixed currencies",
			params: auction.NewAuctionParams{
				ID: uuid.New(), IssuerID: uuid.New(), Lots: []auction.Lot{testLot(), eur},
				OpensAt: testOpens, ClosesAt: testClose,
			},
			wantField: "lots",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			a, err := auction.NewAuction(tc.params, testNow)
			require.Nil(t, a)
			require.ErrorIs(t, err, apperr.ErrValidation)
			assert.Contains(t, fieldNames(apperr.Fields(err)), tc.wantField)
		})
	}
}

func TestLotValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		mutate    func(*auction.Lot)
		wantField string
	}{
		{name: "nil id", mutate: func(l *auction.Lot) { l.ID = uuid.Nil }, wantField: "lot.id"},
		{name: "nil invoice", mutate: func(l *auction.Lot) { l.InvoiceID = uuid.Nil }, wantField: "lot.invoice_id"},
		{name: "nil asset", mutate: func(l *auction.Lot) { l.AssetID = uuid.Nil }, wantField: "lot.asset_id"},
		{name: "nil issuer", mutate: func(l *auction.Lot) { l.IssuerID = uuid.Nil }, wantField: "lot.issuer_id"},
		{name: "empty debtor", mutate: func(l *auction.Lot) { l.DebtorRef = "  " }, wantField: "lot.debtor_ref"},
		{name: "unknown grade", mutate: func(l *auction.Lot) { l.Grade = risk.Grade("Z") }, wantField: "lot.grade"},
		{name: "zero tenor", mutate: func(l *auction.Lot) { l.TenorDays = 0 }, wantField: "lot.tenor_days"},
		{name: "zero supply", mutate: func(l *auction.Lot) { l.Supply = money.Zero(money.USD) }, wantField: "lot.supply"},
		{name: "supply without currency", mutate: func(l *auction.Lot) { l.Supply = money.Amount{} }, wantField: "lot.supply"},
		{name: "zero price", mutate: func(l *auction.Lot) { l.ReservePrice = money.Zero(money.USD) }, wantField: "lot.reserve_price"},
		{name: "price in another currency", mutate: func(l *auction.Lot) {
			l.ReservePrice = money.MustParse("9755.32", money.EUR)
		}, wantField: "lot.reserve_price"},
		{name: "price at face leaves no yield", mutate: func(l *auction.Lot) {
			l.ReservePrice = l.Supply
		}, wantField: "lot.reserve_price"},
		{name: "price above face", mutate: func(l *auction.Lot) {
			l.ReservePrice = money.MustParse("10500.00", money.USD)
		}, wantField: "lot.reserve_price"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			lot := testLot()
			tc.mutate(&lot)

			err := lot.Validate()
			require.ErrorIs(t, err, apperr.ErrValidation)
			assert.Contains(t, fieldNames(apperr.Fields(err)), tc.wantField)
		})
	}
}

func TestLotEconomics(t *testing.T) {
	t.Parallel()

	lot := testLot()

	unitPrice, err := lot.UnitPrice()
	require.NoError(t, err)
	assert.Equal(t, "0.975532", unitPrice.StringFixed(6))

	// (10000 - 9755.32) / 9755.32 * 365 / 60
	yield, err := lot.ImpliedYield()
	require.NoError(t, err)
	assert.Equal(t, "0.152580", yield.StringFixed(6))

	cost, err := lot.CostOf(money.MustParse("2500.00", money.USD))
	require.NoError(t, err)
	assert.Equal(t, "2438.83", cost.String())

	whole, err := lot.CostOf(lot.Supply)
	require.NoError(t, err)
	assert.Equal(t, lot.ReservePrice.String(), whole.String(), "buying the whole lot costs the reserve price")
}

func TestLongerTenorLowersImpliedYield(t *testing.T) {
	t.Parallel()

	short := testLot()
	long := testLot()
	long.TenorDays = 180

	shortYield, err := short.ImpliedYield()
	require.NoError(t, err)
	longYield, err := long.ImpliedYield()
	require.NoError(t, err)

	assert.Equal(t, -1, longYield.Cmp(shortYield), "the same discount over a longer wait annualizes lower")
}

func TestIsAcceptingBids(t *testing.T) {
	t.Parallel()

	a := newAuction(t)
	assert.False(t, a.IsAcceptingBids(testNow), "a draft takes no bids")

	require.NoError(t, a.Open(testOpens))
	assert.True(t, a.IsAcceptingBids(testOpens))
	assert.True(t, a.IsAcceptingBids(testClose.Add(-time.Second)))
	assert.False(t, a.IsAcceptingBids(testClose), "bidding stops at the closing time")
	assert.False(t, a.IsAcceptingBids(testOpens.Add(-time.Second)))
}

func fieldNames(fields []*apperr.FieldError) []string {
	names := make([]string, 0, len(fields))
	for _, f := range fields {
		names = append(names, f.Field)
	}
	return names
}
