package auction_test

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/skimer2king/factorflow/internal/auction"
	"github.com/skimer2king/factorflow/internal/platform/money"
	"github.com/skimer2king/factorflow/internal/risk"
)

// verifiableScenario is a batch with two lots and one large bid, so a test can move
// allocations around and still have somewhere to move them to.
func verifiableScenario(t *testing.T) (*scenario, *auction.Solution) {
	t.Helper()

	s := newScenario(t,
		lot(1, "ACME", issuerA(), "10000.00", "9700.00", risk.GradeB, 60),
		lot(2, "BOLT", issuerB(), "10000.00", "9700.00", risk.GradeB, 60),
	)
	s.bid(1, auction.NewBidParams{
		Budget:   money.MustParse("25000.00", money.USD),
		MinYield: money.MustParseRate("0.05"),
	})

	return s, s.clear()
}

func TestVerifyAcceptsAGenuineSolution(t *testing.T) {
	t.Parallel()

	s, solution := verifiableScenario(t)
	require.NoError(t, auction.Verify(s.auction, s.bids, solution, auction.SolverParamsV1()))
}

// TestVerifyRejectsTamperedSolutions is the safety net the specification requires: the
// verifier recomputes everything, so a solver that overspends, over-allocates or ignores a
// constraint is caught before settlement rather than after.
func TestVerifyRejectsTamperedSolutions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		tamper func(*testing.T, *scenario, *auction.Solution)
		want   string
	}{
		{
			name: "price does not match the notional",
			tamper: func(t *testing.T, _ *scenario, sol *auction.Solution) {
				t.Helper()
				sol.Allocations[0].Price = money.MustParse("1.00", money.USD)
			},
			want: "expected",
		},
		{
			name: "more notional than the lot has supply",
			tamper: func(t *testing.T, s *scenario, sol *auction.Solution) {
				t.Helper()

				lot := lotByID(t, s, sol.Allocations[0].LotID)
				doubled, err := lot.Supply.MulInt(2)
				require.NoError(t, err)
				price, err := lot.CostOf(doubled)
				require.NoError(t, err)

				// The allocation stays internally consistent: only the lot's supply is
				// exceeded, so this is the check that must catch it.
				sol.Allocations[0].Notional = doubled
				sol.Allocations[0].Price = price
			},
			want: "supply",
		},
		{
			name: "the same lot is sold to the same bid twice",
			tamper: func(t *testing.T, _ *scenario, sol *auction.Solution) {
				t.Helper()
				duplicate := sol.Allocations[0]
				duplicate.Rank = len(sol.Allocations)
				sol.Allocations = append(sol.Allocations, duplicate)
			},
			want: "twice",
		},
		{
			name: "an unknown lot",
			tamper: func(t *testing.T, _ *scenario, sol *auction.Solution) {
				t.Helper()
				sol.Allocations[0].LotID = uuid.New()
			},
			want: "unknown lot",
		},
		{
			name: "an unknown bid",
			tamper: func(t *testing.T, _ *scenario, sol *auction.Solution) {
				t.Helper()
				sol.Allocations[0].BidID = uuid.New()
			},
			want: "unknown or inactive bid",
		},
		{
			name: "a bid that was cancelled before clearing",
			tamper: func(t *testing.T, s *scenario, _ *auction.Solution) {
				t.Helper()
				require.NoError(t, s.bids[0].Cancel())
			},
			want: "unknown or inactive bid",
		},
		{
			name: "ranks out of order",
			tamper: func(t *testing.T, _ *scenario, sol *auction.Solution) {
				t.Helper()
				sol.Allocations[0].Rank = 7
			},
			want: "rank",
		},
		{
			name: "a grade the investor refused",
			tamper: func(t *testing.T, s *scenario, _ *auction.Solution) {
				t.Helper()
				s.bids[0].MaxGrade = risk.GradeA
			},
			want: "accepts at most",
		},
		{
			name: "a maturity the investor refused",
			tamper: func(t *testing.T, s *scenario, _ *auction.Solution) {
				t.Helper()
				s.bids[0].MaxTenorDays = 10
			},
			want: "matures in",
		},
		{
			name: "a yield below what the investor demanded",
			tamper: func(t *testing.T, s *scenario, _ *auction.Solution) {
				t.Helper()
				s.bids[0].MinYield = money.MustParseRate("0.90")
			},
			want: "requires",
		},
		{
			name: "an allocation below the investor's minimum lot",
			tamper: func(t *testing.T, s *scenario, _ *auction.Solution) {
				t.Helper()
				s.bids[0].MinimumLot = money.MustParse("9999999.00", money.USD)
			},
			want: "minimum",
		},
		{
			name: "a budget the allocation overspends",
			tamper: func(t *testing.T, s *scenario, _ *auction.Solution) {
				t.Helper()
				s.bids[0].Budget = money.MustParse("100.00", money.USD)
			},
			want: "budget",
		},
		{
			name: "an issuer exposure limit that no longer holds",
			tamper: func(t *testing.T, s *scenario, _ *auction.Solution) {
				t.Helper()
				s.bids[0].MaxIssuerShare = money.MustParseRate("0.01")
			},
			want: auction.ConstraintIssuerExposure.String(),
		},
		{
			name: "a debtor exposure limit that no longer holds",
			tamper: func(t *testing.T, s *scenario, _ *auction.Solution) {
				t.Helper()
				s.bids[0].MaxDebtorShare = money.MustParseRate("0.01")
			},
			want: auction.ConstraintDebtorExposure.String(),
		},
		{
			name: "a grade exposure limit that no longer holds",
			tamper: func(t *testing.T, s *scenario, _ *auction.Solution) {
				t.Helper()
				s.bids[0].MaxGradeShare = map[risk.Grade]money.Rate{risk.GradeB: money.MustParseRate("0.01")}
			},
			want: auction.ConstraintGradeExposure.String(),
		},
		{
			name: "a claimed total that does not match the allocations",
			tamper: func(t *testing.T, _ *scenario, sol *auction.Solution) {
				t.Helper()
				sol.TotalCash = money.MustParse("1.00", money.USD)
			},
			want: "cash total",
		},
		{
			name: "a claimed objective that does not match the allocations",
			tamper: func(t *testing.T, _ *scenario, sol *auction.Solution) {
				t.Helper()
				sol.Objective += 1_000_000
			},
			want: "objective",
		},
		{
			name: "a bid that is both allocated and rejected",
			tamper: func(t *testing.T, _ *scenario, sol *auction.Solution) {
				t.Helper()
				sol.Rejections = append(sol.Rejections, auction.Rejection{
					BidID:      sol.Allocations[0].BidID,
					Constraint: auction.ConstraintBudget,
				})
			},
			want: "allocated and rejected",
		},
		{
			name: "a rejection with no reason",
			tamper: func(t *testing.T, _ *scenario, sol *auction.Solution) {
				t.Helper()
				sol.Rejections = append(sol.Rejections, auction.Rejection{BidID: uuid.New()})
			},
			want: "without a reason",
		},
		{
			name: "a solution for another auction",
			tamper: func(t *testing.T, _ *scenario, sol *auction.Solution) {
				t.Helper()
				sol.AuctionID = uuid.New()
			},
			want: "solution is for auction",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			s, solution := verifiableScenario(t)
			tc.tamper(t, s, solution)

			err := auction.Verify(s.auction, s.bids, solution, auction.SolverParamsV1())
			require.ErrorIs(t, err, auction.ErrVerification)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestVerifyRejectsMissingArguments(t *testing.T) {
	t.Parallel()

	s, solution := verifiableScenario(t)

	require.ErrorIs(t, auction.Verify(nil, s.bids, solution, auction.SolverParamsV1()), auction.ErrVerification)
	require.ErrorIs(t, auction.Verify(s.auction, s.bids, nil, auction.SolverParamsV1()), auction.ErrVerification)
}

func TestVerifyAcceptsAnEmptyAllocation(t *testing.T) {
	t.Parallel()

	s := newScenario(t, lot(1, "ACME", issuerA(), "10000.00", "9755.32", risk.GradeB, 60))
	s.bid(1, auction.NewBidParams{
		Budget:   money.MustParse("50000.00", money.USD),
		MinYield: money.MustParseRate("0.90"),
	})

	solution := s.clear()
	require.Empty(t, solution.Allocations)
	require.Len(t, solution.Rejections, 1)
	require.NoError(t, auction.Verify(s.auction, s.bids, solution, auction.SolverParamsV1()))
}

func lotByID(t *testing.T, s *scenario, id uuid.UUID) auction.Lot {
	t.Helper()

	lot, ok := s.auction.Lot(id)
	require.True(t, ok, "lot %s is not part of the auction", id)
	return lot
}
