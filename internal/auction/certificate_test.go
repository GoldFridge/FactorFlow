package auction_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoldFridge/factorflow/internal/auction"
	"github.com/GoldFridge/factorflow/internal/platform/money"
	"github.com/GoldFridge/factorflow/internal/risk"
)

func certifiedScenario(t *testing.T) (*scenario, *auction.Solution) {
	t.Helper()

	s := newScenario(t,
		lot(1, "ACME", issuerA(), "10000.00", "9755.32", risk.GradeB, 60),
		lot(2, "BOLT", issuerB(), "8000.00", "7790.00", risk.GradeC, 45),
	)
	s.bid(1, auction.NewBidParams{
		Budget: money.MustParse("12000.00", money.USD), MinYield: money.MustParseRate("0.08"),
	})
	s.bid(2, auction.NewBidParams{
		Budget: money.MustParse("9000.00", money.USD), MinYield: money.MustParseRate("0.05"),
		MaxGradeShare: map[risk.Grade]money.Rate{risk.GradeB: money.MustParseRate("0.60")},
	})

	return s, s.clear()
}

// TestCertificateIsStable is the reproducibility claim: rerunning the same clearing on the
// same inputs must produce the same hash, so a judge can check a published allocation.
func TestCertificateIsStable(t *testing.T) {
	t.Parallel()

	s, solution := certifiedScenario(t)
	params := auction.SolverParamsV1()

	first := auction.Certificate(s.auction, s.bids, solution, params)
	assert.Equal(t, solution.CertificateHash, first, "the solver publishes the hash it computes")
	assert.Regexp(t, `^0x[0-9a-f]{64}$`, first)

	for range 10 {
		assert.Equal(t, first, auction.Certificate(s.auction, s.bids, solution, params))
	}

	rerun := s.clear()
	assert.Equal(t, first, rerun.CertificateHash, "a rerun of the same batch certifies identically")
}

// TestCertificateCoversEveryInput checks that the hash actually binds what it claims to:
// change any input or output and the certificate must change with it.
func TestCertificateCoversEveryInput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*testing.T, *scenario, *auction.Solution, *auction.SolverParams)
	}{
		{
			name: "a bid's budget",
			mutate: func(t *testing.T, s *scenario, _ *auction.Solution, _ *auction.SolverParams) {
				t.Helper()
				s.bids[0].Budget = money.MustParse("12500.00", money.USD)
			},
		},
		{
			name: "a bid's minimum yield",
			mutate: func(t *testing.T, s *scenario, _ *auction.Solution, _ *auction.SolverParams) {
				t.Helper()
				s.bids[0].MinYield = money.MustParseRate("0.09")
			},
		},
		{
			name: "a bid's grade limits",
			mutate: func(t *testing.T, s *scenario, _ *auction.Solution, _ *auction.SolverParams) {
				t.Helper()
				s.bids[1].MaxGradeShare[risk.GradeC] = money.MustParseRate("0.10")
			},
		},
		{
			name: "a lot's reserve price",
			mutate: func(t *testing.T, s *scenario, _ *auction.Solution, _ *auction.SolverParams) {
				t.Helper()
				s.auction.Lots[0].ReservePrice = money.MustParse("9700.00", money.USD)
			},
		},
		{
			name: "a lot's grade",
			mutate: func(t *testing.T, s *scenario, _ *auction.Solution, _ *auction.SolverParams) {
				t.Helper()
				s.auction.Lots[0].Grade = risk.GradeC
			},
		},
		{
			name: "an allocation",
			mutate: func(t *testing.T, _ *scenario, sol *auction.Solution, _ *auction.SolverParams) {
				t.Helper()
				sol.Allocations[0].Notional = money.MustParse("1234.00", money.USD)
			},
		},
		{
			name: "the objective",
			mutate: func(t *testing.T, _ *scenario, sol *auction.Solution, _ *auction.SolverParams) {
				t.Helper()
				sol.Objective++
			},
		},
		{
			name: "a solver weight",
			mutate: func(t *testing.T, _ *scenario, _ *auction.Solution, params *auction.SolverParams) {
				t.Helper()
				params.FundingWeight = money.MustParseRate("0.02")
			},
		},
		{
			name: "a solver limit",
			mutate: func(t *testing.T, _ *scenario, _ *auction.Solution, params *auction.SolverParams) {
				t.Helper()
				params.MaxBranchNodes = 1024
			},
		},
		{
			name: "the solver version",
			mutate: func(t *testing.T, _ *scenario, sol *auction.Solution, _ *auction.SolverParams) {
				t.Helper()
				sol.SolverVersion = "solver-v2"
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			s, solution := certifiedScenario(t)
			params := auction.SolverParamsV1()
			before := auction.Certificate(s.auction, s.bids, solution, params)

			tc.mutate(t, s, solution, &params)

			assert.NotEqual(t, before, auction.Certificate(s.auction, s.bids, solution, params))
		})
	}
}

func TestCertificateIgnoresInputOrder(t *testing.T) {
	t.Parallel()

	s, solution := certifiedScenario(t)
	params := auction.SolverParamsV1()
	before := auction.Certificate(s.auction, s.bids, solution, params)

	s.auction.Lots[0], s.auction.Lots[1] = s.auction.Lots[1], s.auction.Lots[0]
	s.bids[0], s.bids[1] = s.bids[1], s.bids[0]

	assert.Equal(t, before, auction.Certificate(s.auction, s.bids, solution, params),
		"the certificate hashes the canonical order, not the order rows arrived in")
}

func TestEmptyBatchStillCertifies(t *testing.T) {
	t.Parallel()

	s := newScenario(t, lot(1, "ACME", issuerA(), "10000.00", "9755.32", risk.GradeB, 60))
	solution := s.clear()

	require.Empty(t, solution.Allocations)
	assert.Regexp(t, `^0x[0-9a-f]{64}$`, solution.CertificateHash,
		"a batch that matched nothing is still certified")
}
