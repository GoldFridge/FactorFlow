package auction_test

import (
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoldFridge/factorflow/internal/auction"
	"github.com/GoldFridge/factorflow/internal/platform/money"
	"github.com/GoldFridge/factorflow/internal/risk"
)

// scenario builds an auction and its bids with stable identifiers, so a test can talk
// about "lot A" and "bid 1" and still get canonical ordering.
type scenario struct {
	t        *testing.T
	auction  *auction.Auction
	bids     []*auction.Bid
	lotIndex map[string]auction.Lot
}

func newScenario(t *testing.T, lots ...auction.Lot) *scenario {
	t.Helper()

	a, err := auction.NewAuction(auction.NewAuctionParams{
		ID:       uuid.MustParse("11111111-1111-4111-8111-111111111111"),
		IssuerID: uuid.MustParse("22222222-2222-4222-8222-222222222222"),
		Lots:     lots,
		OpensAt:  testOpens,
		ClosesAt: testClose,
	}, testNow)
	require.NoError(t, err)

	index := make(map[string]auction.Lot, len(lots))
	for _, lot := range lots {
		index[lot.DebtorRef] = lot
	}
	return &scenario{t: t, auction: a, lotIndex: index}
}

// lot builds a lot with deterministic ids derived from its sequence number.
func lot(seq int, debtor string, issuer uuid.UUID, face, price string, grade risk.Grade, tenor int64) auction.Lot {
	return auction.Lot{
		ID:           seqUUID(0x10, seq),
		InvoiceID:    seqUUID(0x20, seq),
		AssetID:      seqUUID(0x30, seq),
		IssuerID:     issuer,
		DebtorRef:    debtor,
		Supply:       money.MustParse(face, money.USD),
		ReservePrice: money.MustParse(price, money.USD),
		Grade:        grade,
		TenorDays:    tenor,
	}
}

func (s *scenario) bid(seq int, p auction.NewBidParams) *auction.Bid {
	s.t.Helper()

	p.ID = seqUUID(0x40, seq)
	p.AuctionID = s.auction.ID
	if p.InvestorID == uuid.Nil {
		p.InvestorID = seqUUID(0x50, seq)
	}
	if p.MaxGrade == "" {
		p.MaxGrade = risk.GradeE
	}
	if p.MaxTenorDays == 0 {
		p.MaxTenorDays = 365
	}
	if !p.MinimumLot.IsValid() {
		p.MinimumLot = money.Zero(money.USD)
	}

	bid, err := auction.NewBid(p, testNow.Add(time.Duration(seq)*time.Minute))
	require.NoError(s.t, err)
	s.bids = append(s.bids, bid)
	return bid
}

func (s *scenario) clear() *auction.Solution {
	s.t.Helper()

	solution, err := auction.NewSolver().Clear(s.auction, s.bids, testClose)
	require.NoError(s.t, err)
	require.True(s.t, solution.Verified, "the solver only returns solutions its verifier accepted")
	return solution
}

func seqUUID(prefix byte, seq int) uuid.UUID {
	var id uuid.UUID
	id[0] = prefix
	id[15] = byte(seq)
	id[6] = 0x40 | (id[6] & 0x0f)
	id[8] = 0x80 | (id[8] & 0x3f)
	return id
}

func issuerA() uuid.UUID { return seqUUID(0x60, 1) }
func issuerB() uuid.UUID { return seqUUID(0x60, 2) }

// allocationFor returns the allocation of one lot to one bid, if any.
func allocationFor(s *auction.Solution, lotID, bidID uuid.UUID) (auction.Allocation, bool) {
	for _, a := range s.Allocations {
		if a.LotID == lotID && a.BidID == bidID {
			return a, true
		}
	}
	return auction.Allocation{}, false
}

func TestClearAllocatesTheWholeLotToASingleWillingBid(t *testing.T) {
	t.Parallel()

	l := lot(1, "ACME", issuerA(), "10000.00", "9755.32", risk.GradeB, 60)
	s := newScenario(t, l)
	b := s.bid(1, auction.NewBidParams{
		Budget:   money.MustParse("50000.00", money.USD),
		MinYield: money.MustParseRate("0.10"),
	})

	solution := s.clear()

	require.Len(t, solution.Allocations, 1)
	allocation := solution.Allocations[0]
	assert.Equal(t, b.ID, allocation.BidID)
	assert.Equal(t, "10000.00", allocation.Notional.String(), "the whole supply is financed")
	assert.Equal(t, "9755.32", allocation.Price.String(), "at the reserve price")
	assert.Empty(t, solution.Rejections)
	assert.Equal(t, auction.SolverVersionV1, solution.SolverVersion)
	assert.Regexp(t, `^0x[0-9a-f]{64}$`, solution.CertificateHash)
}

func TestBudgetLimitsTheAllocation(t *testing.T) {
	t.Parallel()

	l := lot(1, "ACME", issuerA(), "10000.00", "9755.32", risk.GradeB, 60)
	s := newScenario(t, l)
	b := s.bid(1, auction.NewBidParams{
		Budget:   money.MustParse("2000.00", money.USD),
		MinYield: money.MustParseRate("0.10"),
	})

	solution := s.clear()

	require.Len(t, solution.Allocations, 1)
	allocation := solution.Allocations[0]
	assert.Equal(t, b.ID, allocation.BidID)

	cmp, err := allocation.Price.Cmp(b.Budget)
	require.NoError(t, err)
	assert.LessOrEqual(t, cmp, 0, "an investor never spends more than their budget")
	assert.Equal(t, "2000.00", allocation.Price.String())
	assert.Equal(t, "2050.16", allocation.Notional.String(), "2000 buys slightly more than 2000 of face")
}

// TestSurplusDecidesBetweenCompetingBids checks allocative efficiency: with the price fixed
// at the reserve, the lot goes to the investor who values it most.
func TestSurplusDecidesBetweenCompetingBids(t *testing.T) {
	t.Parallel()

	l := lot(1, "ACME", issuerA(), "10000.00", "9755.32", risk.GradeB, 60)
	s := newScenario(t, l)

	picky := s.bid(1, auction.NewBidParams{
		Budget:   money.MustParse("50000.00", money.USD),
		MinYield: money.MustParseRate("0.15"),
	})
	eager := s.bid(2, auction.NewBidParams{
		Budget:   money.MustParse("50000.00", money.USD),
		MinYield: money.MustParseRate("0.05"),
	})

	solution := s.clear()

	require.Len(t, solution.Allocations, 1)
	assert.Equal(t, eager.ID, solution.Allocations[0].BidID,
		"the bid with the larger surplus takes the lot")

	require.Len(t, solution.Rejections, 1)
	assert.Equal(t, picky.ID, solution.Rejections[0].BidID)
	assert.Equal(t, auction.ConstraintNoRemainingCapacity, solution.Rejections[0].Constraint,
		"the loser was eligible, it simply ran out of supply to take")
}

func TestIneligibleBidsAreRejectedWithTheirBindingConstraint(t *testing.T) {
	t.Parallel()

	l := lot(1, "ACME", issuerA(), "10000.00", "9755.32", risk.GradeC, 60)
	s := newScenario(t, l)

	tooRisky := s.bid(1, auction.NewBidParams{
		Budget:   money.MustParse("50000.00", money.USD),
		MinYield: money.MustParseRate("0.05"),
		MaxGrade: risk.GradeA,
	})
	tooLong := s.bid(2, auction.NewBidParams{
		Budget:       money.MustParse("50000.00", money.USD),
		MinYield:     money.MustParseRate("0.05"),
		MaxTenorDays: 30,
	})
	tooCheap := s.bid(3, auction.NewBidParams{
		Budget:   money.MustParse("50000.00", money.USD),
		MinYield: money.MustParseRate("0.40"),
	})

	solution := s.clear()

	assert.Empty(t, solution.Allocations, "nothing is feasible, so the batch clears empty")
	require.Len(t, solution.Rejections, 3)

	reasons := map[uuid.UUID]auction.Constraint{}
	for _, rejection := range solution.Rejections {
		reasons[rejection.BidID] = rejection.Constraint
		assert.Equal(t, l.ID, rejection.LotID, "the rejection names the lot that refused the bid")
	}
	assert.Equal(t, auction.ConstraintGrade, reasons[tooRisky.ID])
	assert.Equal(t, auction.ConstraintMaturity, reasons[tooLong.ID])
	assert.Equal(t, auction.ConstraintYield, reasons[tooCheap.ID])
}

// TestIssuerExposureIsEnforcedByTheNetwork covers the concentration limit the flow encodes
// directly as a capacity.
func TestIssuerExposureIsEnforcedByTheNetwork(t *testing.T) {
	t.Parallel()

	s := newScenario(t,
		lot(1, "ACME", issuerA(), "10000.00", "9700.00", risk.GradeB, 60),
		lot(2, "BOLT", issuerA(), "10000.00", "9700.00", risk.GradeB, 60),
	)
	b := s.bid(1, auction.NewBidParams{
		Budget:         money.MustParse("50000.00", money.USD),
		MinYield:       money.MustParseRate("0.05"),
		MaxIssuerShare: money.MustParseRate("0.20"),
	})

	solution := s.clear()

	limit, err := b.IssuerLimit()
	require.NoError(t, err)

	spent := money.Zero(money.USD)
	for _, allocation := range solution.Allocations {
		spent, err = spent.Add(allocation.Price)
		require.NoError(t, err)
	}
	cmp, err := spent.Cmp(limit)
	require.NoError(t, err)
	assert.LessOrEqual(t, cmp, 0, "spent %s against a %s single-issuer limit", spent, limit)
	assert.Equal(t, "10000.00", limit.String())
}

// TestDebtorExposureIsRepaired covers the dimension a single flow cannot express, which
// the specification's repair step handles instead.
func TestDebtorExposureIsRepaired(t *testing.T) {
	t.Parallel()

	s := newScenario(t,
		lot(1, "ACME", issuerA(), "10000.00", "9700.00", risk.GradeB, 60),
		lot(2, "ACME", issuerB(), "10000.00", "9700.00", risk.GradeB, 60),
	)
	b := s.bid(1, auction.NewBidParams{
		Budget:         money.MustParse("50000.00", money.USD),
		MinYield:       money.MustParseRate("0.05"),
		MaxDebtorShare: money.MustParseRate("0.20"),
	})

	solution := s.clear()

	limit, err := b.DebtorLimit()
	require.NoError(t, err)

	spent := money.Zero(money.USD)
	for _, allocation := range solution.Allocations {
		spent, err = spent.Add(allocation.Price)
		require.NoError(t, err)
	}
	cmp, err := spent.Cmp(limit)
	require.NoError(t, err)
	assert.LessOrEqual(t, cmp, 0, "spent %s on one debtor against a %s limit", spent, limit)
	assert.Positive(t, solution.RepairRounds, "the repair loop had to run")
}

func TestGradeExposureIsRepaired(t *testing.T) {
	t.Parallel()

	s := newScenario(t,
		lot(1, "ACME", issuerA(), "10000.00", "9700.00", risk.GradeD, 60),
		lot(2, "BOLT", issuerB(), "10000.00", "9700.00", risk.GradeD, 60),
	)
	b := s.bid(1, auction.NewBidParams{
		Budget:        money.MustParse("50000.00", money.USD),
		MinYield:      money.MustParseRate("0.05"),
		MaxGradeShare: map[risk.Grade]money.Rate{risk.GradeD: money.MustParseRate("0.25")},
	})

	solution := s.clear()

	limit, err := b.GradeLimit(risk.GradeD)
	require.NoError(t, err)

	spent := money.Zero(money.USD)
	for _, allocation := range solution.Allocations {
		spent, err = spent.Add(allocation.Price)
		require.NoError(t, err)
	}
	cmp, err := spent.Cmp(limit)
	require.NoError(t, err)
	assert.LessOrEqual(t, cmp, 0, "spent %s on grade D against a %s limit", spent, limit)
}

// TestMinimumLotIsAllOrNothing is the discrete constraint the branch and bound exists for:
// an investor either gets at least their minimum or nothing at all.
func TestMinimumLotIsAllOrNothing(t *testing.T) {
	t.Parallel()

	s := newScenario(t, lot(1, "ACME", issuerA(), "10000.00", "9755.32", risk.GradeB, 60))

	big := s.bid(1, auction.NewBidParams{
		Budget:   money.MustParse("9000.00", money.USD),
		MinYield: money.MustParseRate("0.05"),
	})
	fussy := s.bid(2, auction.NewBidParams{
		Budget:     money.MustParse("9000.00", money.USD),
		MinYield:   money.MustParseRate("0.05"),
		MinimumLot: money.MustParse("5000.00", money.USD),
	})

	solution := s.clear()

	if allocation, ok := allocationFor(solution, s.lotIndex["ACME"].ID, fussy.ID); ok {
		cmp, err := allocation.Notional.Cmp(fussy.MinimumLot)
		require.NoError(t, err)
		assert.GreaterOrEqual(t, cmp, 0, "an allocation below the minimum lot must not happen")
	}

	total := money.Zero(money.USD)
	for _, allocation := range solution.Allocations {
		var err error
		total, err = total.Add(allocation.Notional)
		require.NoError(t, err)
	}
	cmp, err := total.Cmp(s.lotIndex["ACME"].Supply)
	require.NoError(t, err)
	assert.LessOrEqual(t, cmp, 0)
	assert.NotEmpty(t, big.ID)
}

func TestClearIsDeterministicRegardlessOfInputOrder(t *testing.T) {
	t.Parallel()

	build := func() *scenario {
		s := newScenario(t,
			lot(1, "ACME", issuerA(), "10000.00", "9755.32", risk.GradeB, 60),
			lot(2, "BOLT", issuerB(), "8000.00", "7790.00", risk.GradeC, 45),
			lot(3, "CIRRUS", issuerA(), "12000.00", "11600.00", risk.GradeB, 90),
		)
		s.bid(1, auction.NewBidParams{
			Budget: money.MustParse("15000.00", money.USD), MinYield: money.MustParseRate("0.08"),
			MaxGrade: risk.GradeB, MaxIssuerShare: money.MustParseRate("0.60"),
		})
		s.bid(2, auction.NewBidParams{
			Budget: money.MustParse("9000.00", money.USD), MinYield: money.MustParseRate("0.05"),
			MaxDebtorShare: money.MustParseRate("0.50"),
		})
		s.bid(3, auction.NewBidParams{
			Budget: money.MustParse("20000.00", money.USD), MinYield: money.MustParseRate("0.10"),
			MinimumLot: money.MustParse("2500.00", money.USD),
		})
		return s
	}

	reference := build().clear()

	for round := range 5 {
		shuffled := build()
		rng := rand.New(rand.NewSource(int64(round) + 1))
		rng.Shuffle(len(shuffled.auction.Lots), func(i, j int) {
			shuffled.auction.Lots[i], shuffled.auction.Lots[j] = shuffled.auction.Lots[j], shuffled.auction.Lots[i]
		})
		rng.Shuffle(len(shuffled.bids), func(i, j int) {
			shuffled.bids[i], shuffled.bids[j] = shuffled.bids[j], shuffled.bids[i]
		})

		got := shuffled.clear()
		require.Equalf(t, reference.CertificateHash, got.CertificateHash,
			"round %d produced a different certificate", round)
		require.Equal(t, reference.Objective, got.Objective)
		require.Equal(t, len(reference.Allocations), len(got.Allocations))
		for i := range reference.Allocations {
			require.Equal(t, reference.Allocations[i].BidID, got.Allocations[i].BidID)
			require.Equal(t, reference.Allocations[i].LotID, got.Allocations[i].LotID)
			require.Equal(t, reference.Allocations[i].Notional.String(), got.Allocations[i].Notional.String())
		}
	}
}

func TestCancelledAndForeignBidsAreIgnored(t *testing.T) {
	t.Parallel()

	s := newScenario(t, lot(1, "ACME", issuerA(), "10000.00", "9755.32", risk.GradeB, 60))

	cancelled := s.bid(1, auction.NewBidParams{
		Budget: money.MustParse("50000.00", money.USD), MinYield: money.MustParseRate("0.05"),
	})
	require.NoError(t, cancelled.Cancel())

	late := s.bid(2, auction.NewBidParams{
		Budget: money.MustParse("50000.00", money.USD), MinYield: money.MustParseRate("0.05"),
	})
	late.CreatedAt = testClose.Add(time.Minute)

	solution := s.clear()

	assert.Empty(t, solution.Allocations)
	assert.Empty(t, solution.Rejections, "a bid that cannot take part is not reported as rejected")
}

func TestClearRejectsBidsFromAnotherAuction(t *testing.T) {
	t.Parallel()

	s := newScenario(t, lot(1, "ACME", issuerA(), "10000.00", "9755.32", risk.GradeB, 60))
	stray := s.bid(1, auction.NewBidParams{
		Budget: money.MustParse("50000.00", money.USD), MinYield: money.MustParseRate("0.05"),
	})
	stray.AuctionID = uuid.New()

	_, err := auction.NewSolver().Clear(s.auction, s.bids, testClose)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "belongs to auction")
}

// TestLargeBatchRespectsEveryInvariant runs a pseudo-random batch through the solver and
// its verifier. The seed is fixed, so a failure is reproducible rather than a flake.
func largeScenario(t *testing.T) *scenario {
	t.Helper()

	rng := rand.New(rand.NewSource(20260905))

	lots := make([]auction.Lot, 0, 24)
	for i := range 24 {
		face := 5000 + rng.Intn(20)*500
		discount := 150 + rng.Intn(600)
		grade := []risk.Grade{risk.GradeA, risk.GradeB, risk.GradeC, risk.GradeD}[rng.Intn(4)]
		issuer := []uuid.UUID{issuerA(), issuerB(), seqUUID(0x60, 3)}[rng.Intn(3)]
		debtor := fmt.Sprintf("DEBTOR-%d", rng.Intn(5))

		lots = append(lots, lot(i+1, debtor, issuer,
			fmt.Sprintf("%d.00", face),
			fmt.Sprintf("%d.00", face-discount),
			grade, int64(30+rng.Intn(120))))
	}

	s := newScenario(t, lots...)
	for i := range 12 {
		s.bid(i+1, auction.NewBidParams{
			Budget:         money.MustParse(fmt.Sprintf("%d.00", 5000+rng.Intn(40)*1000), money.USD),
			MinYield:       money.MustParseRate(fmt.Sprintf("0.0%d", 1+rng.Intn(8))),
			MaxGrade:       []risk.Grade{risk.GradeB, risk.GradeC, risk.GradeD}[rng.Intn(3)],
			MaxTenorDays:   int64(60 + rng.Intn(120)),
			MinimumLot:     money.MustParse(fmt.Sprintf("%d.00", rng.Intn(3)*1000), money.USD),
			MaxIssuerShare: money.MustParseRate("0.50"),
			MaxDebtorShare: money.MustParseRate("0.40"),
		})
	}

	return s
}

func TestLargeBatchRespectsEveryInvariant(t *testing.T) {
	t.Parallel()

	s := largeScenario(t)
	solution := s.clear()

	// The verifier already ran inside Clear; assert the headline invariants here too, so a
	// regression names the invariant it broke rather than pointing at the verifier.
	perLot := map[uuid.UUID]money.Amount{}
	perBid := map[uuid.UUID]money.Amount{}
	for _, allocation := range solution.Allocations {
		var err error
		if !perLot[allocation.LotID].IsValid() {
			perLot[allocation.LotID] = money.Zero(money.USD)
		}
		perLot[allocation.LotID], err = perLot[allocation.LotID].Add(allocation.Notional)
		require.NoError(t, err)

		if !perBid[allocation.BidID].IsValid() {
			perBid[allocation.BidID] = money.Zero(money.USD)
		}
		perBid[allocation.BidID], err = perBid[allocation.BidID].Add(allocation.Price)
		require.NoError(t, err)
	}

	for _, l := range s.auction.Lots {
		allocated, ok := perLot[l.ID]
		if !ok {
			continue
		}
		cmp, err := allocated.Cmp(l.Supply)
		require.NoError(t, err)
		require.LessOrEqualf(t, cmp, 0, "lot %s over-allocated: %s of %s", l.ID, allocated, l.Supply)
	}

	for _, b := range s.bids {
		spent, ok := perBid[b.ID]
		if !ok {
			continue
		}
		cmp, err := spent.Cmp(b.Budget)
		require.NoError(t, err)
		require.LessOrEqualf(t, cmp, 0, "bid %s overspent: %s of %s", b.ID, spent, b.Budget)
	}

	assert.NotEmpty(t, solution.Allocations, "a batch this size should match something")
}
