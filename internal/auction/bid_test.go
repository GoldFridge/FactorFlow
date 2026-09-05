package auction_test

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/skimer2king/factorflow/internal/auction"
	"github.com/skimer2king/factorflow/internal/platform/apperr"
	"github.com/skimer2king/factorflow/internal/platform/money"
	"github.com/skimer2king/factorflow/internal/risk"
)

func validBidParams() auction.NewBidParams {
	return auction.NewBidParams{
		ID:             uuid.New(),
		AuctionID:      uuid.New(),
		InvestorID:     uuid.New(),
		Budget:         money.MustParse("50000.00", money.USD),
		MinYield:       money.MustParseRate("0.10"),
		MaxGrade:       risk.GradeC,
		MaxTenorDays:   90,
		MinimumLot:     money.MustParse("1000.00", money.USD),
		MaxIssuerShare: money.MustParseRate("0.40"),
		MaxDebtorShare: money.MustParseRate("0.25"),
		MaxGradeShare:  map[risk.Grade]money.Rate{risk.GradeC: money.MustParseRate("0.30")},
	}
}

func newBid(t *testing.T) *auction.Bid {
	t.Helper()

	bid, err := auction.NewBid(validBidParams(), testNow)
	require.NoError(t, err)
	return bid
}

func TestNewBid(t *testing.T) {
	t.Parallel()

	bid := newBid(t)

	assert.Equal(t, auction.BidStatusActive, bid.Status)
	assert.Equal(t, int64(1), bid.Version)
	assert.Equal(t, testNow, bid.CreatedAt)
}

func TestNewBidRejectsInvalidConstraints(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		mutate    func(*auction.NewBidParams)
		wantField string
	}{
		{name: "nil id", mutate: func(p *auction.NewBidParams) { p.ID = uuid.Nil }, wantField: "id"},
		{name: "nil auction", mutate: func(p *auction.NewBidParams) { p.AuctionID = uuid.Nil }, wantField: "auction_id"},
		{name: "nil investor", mutate: func(p *auction.NewBidParams) { p.InvestorID = uuid.Nil }, wantField: "investor_id"},
		{name: "zero budget", mutate: func(p *auction.NewBidParams) { p.Budget = money.Zero(money.USD) }, wantField: "budget"},
		{name: "budget without currency", mutate: func(p *auction.NewBidParams) { p.Budget = money.Amount{} }, wantField: "budget"},
		{name: "negative yield", mutate: func(p *auction.NewBidParams) { p.MinYield = money.MustParseRate("-0.01") }, wantField: "min_yield"},
		{name: "unknown grade", mutate: func(p *auction.NewBidParams) { p.MaxGrade = risk.Grade("Z") }, wantField: "max_grade"},
		{name: "zero tenor", mutate: func(p *auction.NewBidParams) { p.MaxTenorDays = 0 }, wantField: "max_tenor_days"},
		{name: "negative minimum lot", mutate: func(p *auction.NewBidParams) {
			p.MinimumLot = money.MustParse("-1.00", money.USD)
		}, wantField: "minimum_lot"},
		{name: "minimum lot in another currency", mutate: func(p *auction.NewBidParams) {
			p.MinimumLot = money.MustParse("1000.00", money.EUR)
		}, wantField: "minimum_lot"},
		{name: "issuer share above one", mutate: func(p *auction.NewBidParams) {
			p.MaxIssuerShare = money.MustParseRate("1.5")
		}, wantField: "max_issuer_share"},
		{name: "negative debtor share", mutate: func(p *auction.NewBidParams) {
			p.MaxDebtorShare = money.MustParseRate("-0.1")
		}, wantField: "max_debtor_share"},
		{name: "grade share above one", mutate: func(p *auction.NewBidParams) {
			p.MaxGradeShare = map[risk.Grade]money.Rate{risk.GradeB: money.MustParseRate("2")}
		}, wantField: "max_grade_share.B"},
		{name: "share for an unknown grade", mutate: func(p *auction.NewBidParams) {
			p.MaxGradeShare = map[risk.Grade]money.Rate{risk.Grade("Z"): money.MustParseRate("0.2")}
		}, wantField: "max_grade_share"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			p := validBidParams()
			tc.mutate(&p)

			bid, err := auction.NewBid(p, testNow)
			require.Nil(t, bid)
			require.ErrorIs(t, err, apperr.ErrValidation)
			assert.Contains(t, fieldNames(apperr.Fields(err)), tc.wantField)
		})
	}
}

func TestBidExposureLimits(t *testing.T) {
	t.Parallel()

	bid := newBid(t)

	issuerLimit, err := bid.IssuerLimit()
	require.NoError(t, err)
	assert.Equal(t, "20000.00", issuerLimit.String(), "40% of a 50 000 budget")

	debtorLimit, err := bid.DebtorLimit()
	require.NoError(t, err)
	assert.Equal(t, "12500.00", debtorLimit.String())

	gradeLimit, err := bid.GradeLimit(risk.GradeC)
	require.NoError(t, err)
	assert.Equal(t, "15000.00", gradeLimit.String())

	uncapped, err := bid.GradeLimit(risk.GradeA)
	require.NoError(t, err)
	assert.Equal(t, bid.Budget.String(), uncapped.String(), "a grade with no limit may take the whole budget")
}

func TestOmittedSharesMeanNoLimit(t *testing.T) {
	t.Parallel()

	p := validBidParams()
	p.MaxIssuerShare = money.ZeroRate()
	p.MaxDebtorShare = money.ZeroRate()
	p.MaxGradeShare = nil

	bid, err := auction.NewBid(p, testNow)
	require.NoError(t, err)

	issuerLimit, err := bid.IssuerLimit()
	require.NoError(t, err)
	assert.Equal(t, bid.Budget.String(), issuerLimit.String())

	debtorLimit, err := bid.DebtorLimit()
	require.NoError(t, err)
	assert.Equal(t, bid.Budget.String(), debtorLimit.String())
}

func TestCancelBid(t *testing.T) {
	t.Parallel()

	bid := newBid(t)
	require.NoError(t, bid.Cancel())
	assert.Equal(t, auction.BidStatusCancelled, bid.Status)
	assert.Equal(t, int64(2), bid.Version)

	require.ErrorIs(t, bid.Cancel(), apperr.ErrConflict, "a cancelled bid cannot be cancelled twice")
}

// TestFeasibleReportsTheFirstBindingConstraint is the explainability requirement: a
// rejected bid is told which of its own limits refused the lot.
func TestFeasibleReportsTheFirstBindingConstraint(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		mutateBid  func(*auction.NewBidParams)
		mutateLot  func(*auction.Lot)
		want       auction.Constraint
		wantAllows bool
	}{
		{
			name:       "a lot the bid can take",
			wantAllows: true,
		},
		{
			name:      "another currency",
			mutateLot: func(l *auction.Lot) { l.Supply = money.MustParse("10000.00", money.EUR) },
			want:      auction.ConstraintCurrency,
		},
		{
			name:      "riskier than the investor accepts",
			mutateLot: func(l *auction.Lot) { l.Grade = risk.GradeD },
			want:      auction.ConstraintGrade,
		},
		{
			name:      "longer than the investor accepts",
			mutateLot: func(l *auction.Lot) { l.TenorDays = 120 },
			want:      auction.ConstraintMaturity,
		},
		{
			name:      "yields less than the investor requires",
			mutateLot: func(l *auction.Lot) { l.ReservePrice = money.MustParse("9950.00", money.USD) },
			want:      auction.ConstraintYield,
		},
		{
			name:      "smaller than the investor's minimum lot",
			mutateBid: func(p *auction.NewBidParams) { p.MinimumLot = money.MustParse("25000.00", money.USD) },
			want:      auction.ConstraintMinimumLot,
		},
		{
			name: "minimum lot costs more than the budget",
			mutateBid: func(p *auction.NewBidParams) {
				p.Budget = money.MustParse("500.00", money.USD)
				p.MinimumLot = money.MustParse("1000.00", money.USD)
			},
			want: auction.ConstraintBudget,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			p := validBidParams()
			if tc.mutateBid != nil {
				tc.mutateBid(&p)
			}
			bid, err := auction.NewBid(p, testNow)
			require.NoError(t, err)

			lot := testLot()
			if tc.mutateLot != nil {
				tc.mutateLot(&lot)
			}

			ok, binding, err := bid.Feasible(lot)
			require.NoError(t, err)

			if tc.wantAllows {
				assert.True(t, ok)
				assert.Empty(t, binding)
				return
			}
			assert.False(t, ok)
			assert.Equal(t, tc.want, binding)
		})
	}
}

// TestFeasibilityOrderIsFixed checks that a lot violating several constraints always
// reports the same one, so a rejection reason is reproducible.
func TestFeasibilityOrderIsFixed(t *testing.T) {
	t.Parallel()

	bid := newBid(t)

	lot := testLot()
	lot.Grade = risk.GradeE
	lot.TenorDays = 400
	lot.ReservePrice = money.MustParse("9990.00", money.USD)

	for range 10 {
		ok, binding, err := bid.Feasible(lot)
		require.NoError(t, err)
		require.False(t, ok)
		require.Equal(t, auction.ConstraintGrade, binding, "grade is checked before maturity and yield")
	}
}

func TestZeroMinimumLotSkipsTheSizeChecks(t *testing.T) {
	t.Parallel()

	p := validBidParams()
	p.Budget = money.MustParse("10.00", money.USD)
	p.MinimumLot = money.Zero(money.USD)

	bid, err := auction.NewBid(p, testNow)
	require.NoError(t, err)

	ok, binding, err := bid.Feasible(testLot())
	require.NoError(t, err)
	assert.True(t, ok, "a tiny budget can still take a tiny slice when no minimum lot is set")
	assert.Empty(t, binding)
}
