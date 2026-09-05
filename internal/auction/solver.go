package auction

import (
	"sort"
	"time"

	"github.com/google/uuid"

	"github.com/skimer2king/factorflow/internal/platform/apperr"
	"github.com/skimer2king/factorflow/internal/platform/money"
)

// SolverVersionV1 is the published identifier of the clearing algorithm. It is recorded on
// every cleared auction and hashed into the allocation certificate, because a different
// solver is allowed to reach a different allocation from the same bids.
const SolverVersionV1 = "solver-v1"

// valueScale converts a rate into integer value points per unit of cash, so the objective
// is exact integer arithmetic rather than accumulated floating-point drift.
const valueScale int64 = 1_000_000

// SolverParams are the published weights and limits of the clearing algorithm.
type SolverParams struct {
	// SurplusWeight scales the investor's surplus, the yield a lot offers above the
	// minimum yield the bid demanded.
	SurplusWeight money.Rate
	// FundingWeight rewards financing volume at all, so the solver prefers putting a
	// receivable to work over leaving both cash and supply idle.
	FundingWeight money.Rate
	// RiskPenalty is charged per grade rank, so a safer lot wins a tie against a riskier
	// one offering the same surplus.
	RiskPenalty money.Rate

	// MaxAugmentations bounds the flow phase. It is a count, not a duration, so a slow
	// machine produces the same allocation as a fast one.
	MaxAugmentations int
	// MaxRelaxedSolves is the total number of flow solves one clearing may spend across
	// branching and repair. It is the budget that keeps a large batch inside its time
	// target; the best verified solution found within it is used.
	MaxRelaxedSolves int
	// MaxEdgesPerBid caps how many lots one bid competes for, keeping the largest batches
	// tractable. Only the most valuable eligible lots are kept, in canonical order.
	MaxEdgesPerBid int
	// EdgePruneThreshold is the number of feasible pairs above which MaxEdgesPerBid starts
	// to apply. Below it every eligible pair is considered, so ordinary batches are solved
	// over their whole feasible graph.
	EdgePruneThreshold int
	// MaxBranchNodes bounds the branch and bound over minimum-lot constraints. On
	// exhaustion the best verified solution found so far is used.
	MaxBranchNodes int
	// MaxRepairRounds bounds the concentration repair loop.
	MaxRepairRounds int
}

// SolverParamsV1 returns the published parameters.
func SolverParamsV1() SolverParams {
	return SolverParams{
		SurplusWeight:      money.MustParseRate("1.0"),
		FundingWeight:      money.MustParseRate("0.01"),
		RiskPenalty:        money.MustParseRate("0.002"),
		MaxAugmentations:   200_000,
		MaxRelaxedSolves:   32,
		MaxEdgesPerBid:     32,
		EdgePruneThreshold: 20_000,
		MaxBranchNodes:     512,
		MaxRepairRounds:    32,
	}
}

// Solver clears a batch auction.
//
// It is deterministic by construction: inputs are canonicalized before anything is
// computed, every weight is integer arithmetic, ties are broken by published order, and
// both bounded searches stop on counts rather than on elapsed time.
type Solver struct {
	Version string
	Params  SolverParams
}

// NewSolver returns the published solver.
func NewSolver() *Solver {
	return &Solver{Version: SolverVersionV1, Params: SolverParamsV1()}
}

// Allocation is one investor's share of one lot.
type Allocation struct {
	LotID      uuid.UUID
	InvoiceID  uuid.UUID
	AssetID    uuid.UUID
	BidID      uuid.UUID
	InvestorID uuid.UUID

	// Notional is the face value allocated; Price is the cash the investor pays for it.
	Notional money.Amount
	Price    money.Amount

	// Rank is the allocation's position in canonical order, recorded so the certificate
	// and the settlement plan agree on sequence.
	Rank int
}

// Rejection explains why a bid received nothing.
type Rejection struct {
	BidID uuid.UUID
	// LotID is the lot whose constraint bound first, or the nil UUID when the bid was
	// feasible everywhere but lost on capacity.
	LotID      uuid.UUID
	Constraint Constraint
}

// Solution is the deterministic result of clearing a batch.
type Solution struct {
	AuctionID     uuid.UUID
	SolverVersion string

	Allocations []Allocation
	Rejections  []Rejection

	// Objective is the total value of the allocation in integer value points.
	Objective     int64
	TotalNotional money.Amount
	TotalCash     money.Amount

	// BranchNodes and RepairRounds record how much search the result took, and whether a
	// bound was reached.
	BranchNodes  int
	RepairRounds int
	LimitReached bool

	CertificateHash string
	Verified        bool
}

// Clear runs the batch auction and returns a verified allocation.
//
// A batch with no feasible match is not an error: it clears with an empty allocation and a
// rejection reason per bid. An error means the solver could not produce a solution it
// trusts, which the caller records as a failed clearing.
func (s *Solver) Clear(a *Auction, bids []*Bid, now time.Time) (*Solution, error) {
	p, err := s.canonicalize(a, bids)
	if err != nil {
		return nil, err
	}

	r := &run{problem: p, params: s.Params}
	best, err := s.branchAndBound(r)
	if err != nil {
		return nil, err
	}

	solution, err := s.materializeSolution(p, best)
	if err != nil {
		return nil, err
	}

	if err := Verify(a, bids, solution, s.Params); err != nil {
		return nil, err
	}
	solution.Verified = true
	solution.CertificateHash = Certificate(a, p.bids, solution, s.Params)

	return solution, nil
}

// problem is the canonical form of one clearing run: lots and bids in published order,
// with every feasible pair and its value precomputed.
type problem struct {
	auctionID uuid.UUID
	lots      []Lot
	bids      []*Bid
	edges     []edgeSpec
	// blocked records, per bid, the first binding constraint found while scanning lots in
	// canonical order. It is the explanation a bid that matched nothing receives.
	blocked  []Rejection
	currency money.Currency
	params   SolverParams
}

// edgeSpec is one feasible (lot, bid) pair.
type edgeSpec struct {
	lot, bid int
	// value is the value points earned per unit of cash spent on this pair.
	value int64
	// maxCash is the most cash that could ever flow along the pair.
	maxCash int64
	// unitPrice converts cash back into notional.
	unitPrice money.Rate
	// minimumCash is the cash cost of the bid's minimum lot on this pair, zero when the
	// bid sets no minimum.
	minimumCash int64
}

// canonicalize sorts the inputs into published order, drops bids that cannot take part,
// and computes every feasible edge.
//
// Sorting first is what makes the whole run reproducible: bids are ordered by creation
// time then id, lots by invoice then lot id, which is exactly the tie-break the
// specification publishes.
func (s *Solver) canonicalize(a *Auction, bids []*Bid) (*problem, error) {
	if a == nil {
		return nil, apperr.Invalid("auction", "must not be nil")
	}
	if len(a.Lots) == 0 {
		return nil, apperr.Invalid("auction.lots", "must contain at least one lot")
	}

	lots := append([]Lot(nil), a.Lots...)
	sort.Slice(lots, func(i, j int) bool {
		if lots[i].InvoiceID != lots[j].InvoiceID {
			return lots[i].InvoiceID.String() < lots[j].InvoiceID.String()
		}
		return lots[i].ID.String() < lots[j].ID.String()
	})

	active := make([]*Bid, 0, len(bids))
	for _, bid := range bids {
		if bid == nil || bid.Status != BidStatusActive {
			continue
		}
		if bid.AuctionID != a.ID {
			return nil, apperr.Invalid("bids", "bid %s belongs to auction %s", bid.ID, bid.AuctionID)
		}
		// A bid placed after the auction closed cannot take part, whatever wrote it.
		if bid.CreatedAt.After(a.ClosesAt) {
			continue
		}
		active = append(active, bid)
	}
	sort.Slice(active, func(i, j int) bool {
		if !active[i].CreatedAt.Equal(active[j].CreatedAt) {
			return active[i].CreatedAt.Before(active[j].CreatedAt)
		}
		return active[i].ID.String() < active[j].ID.String()
	})

	p := &problem{
		auctionID: a.ID,
		lots:      lots,
		bids:      active,
		blocked:   make([]Rejection, len(active)),
		currency:  lots[0].Supply.Currency(),
		params:    s.Params,
	}

	for j, bid := range active {
		p.blocked[j] = Rejection{BidID: bid.ID, Constraint: ConstraintNoRemainingCapacity}
		blockedRecorded := false

		for i, lot := range lots {
			feasible, binding, err := bid.Feasible(lot)
			if err != nil {
				return nil, err
			}
			if !feasible {
				if !blockedRecorded {
					p.blocked[j] = Rejection{BidID: bid.ID, LotID: lot.ID, Constraint: binding}
					blockedRecorded = true
				}
				continue
			}

			spec, err := s.edgeFor(lot, bid, i, j)
			if err != nil {
				return nil, err
			}
			p.edges = append(p.edges, spec)
			// A pair that works anywhere means the bid was not blocked outright; capacity
			// is then the only thing that can still refuse it.
			p.blocked[j] = Rejection{BidID: bid.ID, Constraint: ConstraintNoRemainingCapacity}
			blockedRecorded = true
		}
	}
	p.prune(s.Params)

	return p, nil
}

// edgeFor computes the value of one feasible pair.
func (s *Solver) edgeFor(lot Lot, bid *Bid, lotIndex, bidIndex int) (edgeSpec, error) {
	yield, err := lot.ImpliedYield()
	if err != nil {
		return edgeSpec{}, err
	}
	unitPrice, err := lot.UnitPrice()
	if err != nil {
		return edgeSpec{}, err
	}

	surplus := yield.Sub(bid.MinYield)
	value := s.Params.SurplusWeight.Mul(surplus).
		Add(s.Params.FundingWeight).
		Sub(s.Params.RiskPenalty.MulInt(int64(lot.Grade.Rank())))

	valuePoints := value.Mul(money.RateFromInt(valueScale)).Decimal().RoundBank(0).IntPart()
	if valuePoints < 0 {
		valuePoints = 0
	}

	maxCash := lot.ReservePrice.Minor()
	if bid.Budget.Minor() < maxCash {
		maxCash = bid.Budget.Minor()
	}

	var minimumCash int64
	if bid.MinimumLot.IsPositive() {
		cost, err := lot.CostOf(bid.MinimumLot)
		if err != nil {
			return edgeSpec{}, err
		}
		minimumCash = cost.Minor()
	}

	return edgeSpec{
		lot:         lotIndex,
		bid:         bidIndex,
		value:       valuePoints,
		maxCash:     maxCash,
		unitPrice:   unitPrice,
		minimumCash: minimumCash,
	}, nil
}

// state is a node of the branch and bound: which edges are forbidden, and which have been
// pre-assigned their minimum lot.
type state struct {
	forbidden map[int]bool
	preassign map[int]int64
	// capped holds tighter capacities imposed by the concentration repair, in cash units.
	capped map[int]int64
	// forced marks edges the search has committed to at least their minimum lot, so a
	// later repair pass cannot quietly drop them again.
	forced map[int]bool
}

func newState() *state {
	return &state{
		forbidden: map[int]bool{},
		preassign: map[int]int64{},
		capped:    map[int]int64{},
		forced:    map[int]bool{},
	}
}

func (s *state) clone() *state {
	next := &state{
		forbidden: make(map[int]bool, len(s.forbidden)+1),
		preassign: make(map[int]int64, len(s.preassign)+1),
		capped:    make(map[int]int64, len(s.capped)+1),
		forced:    make(map[int]bool, len(s.forced)+1),
	}
	for k, v := range s.forbidden {
		next.forbidden[k] = v
	}
	for k, v := range s.preassign {
		next.preassign[k] = v
	}
	for k, v := range s.capped {
		next.capped[k] = v
	}
	for k, v := range s.forced {
		next.forced[k] = v
	}
	return next
}

// capacityOf is the most cash an edge may carry under this branch's constraints.
func (s *state) capacityOf(index int, spec edgeSpec) int64 {
	capacity := spec.maxCash
	if capped, ok := s.capped[index]; ok && capped < capacity {
		capacity = capped
	}
	return capacity
}

// rawSolution is an allocation in cash units, before it is converted into notional.
type rawSolution struct {
	cash []int64
	// dropped lists edges the minimum-lot repair removed; they are the branch points the
	// search revisits when looking for a better allocation.
	dropped      []int
	objective    int64
	branchNodes  int
	repairRounds int
	limitReached bool
}

// run is the mutable state of one clearing: the canonical problem plus the search budget
// shared by branching and repair.
//
// The budget is a count of flow solves, not a deadline, so a slow machine and a fast one
// explore exactly the same search tree and reach the same allocation.
type run struct {
	problem   *problem
	params    SolverParams
	solves    int
	exhausted bool
}

// spent reports whether the search budget is used up. It is consulted between branch and
// bound nodes; a node already under way always finishes, so a partially repaired
// allocation can never escape the search.
func (r *run) spent() bool {
	if r.solves >= r.params.MaxRelaxedSolves {
		r.exhausted = true
		return true
	}
	return false
}

// prune keeps only the most valuable eligible lots per bid once the feasible graph grows
// past the published threshold.
//
// This is a documented limit, not a hidden heuristic: it bounds the search, it is applied
// in canonical order so it is reproducible, and it removes only edges, never constraints.
// Every allocation that survives is still checked in full by the verifier.
func (p *problem) prune(params SolverParams) {
	if params.MaxEdgesPerBid <= 0 || len(p.edges) <= params.EdgePruneThreshold {
		return
	}

	byBid := make([][]edgeSpec, len(p.bids))
	for _, spec := range p.edges {
		byBid[spec.bid] = append(byBid[spec.bid], spec)
	}

	kept := make([]edgeSpec, 0, len(p.bids)*params.MaxEdgesPerBid)
	for _, specs := range byBid {
		if len(specs) > params.MaxEdgesPerBid {
			sort.SliceStable(specs, func(i, j int) bool {
				if specs[i].value != specs[j].value {
					return specs[i].value > specs[j].value
				}
				return specs[i].lot < specs[j].lot
			})
			specs = specs[:params.MaxEdgesPerBid]
		}
		kept = append(kept, specs...)
	}

	// Restore canonical (bid, lot) order: the edge index is the solver's tie-break.
	sort.SliceStable(kept, func(i, j int) bool {
		if kept[i].bid != kept[j].bid {
			return kept[i].bid < kept[j].bid
		}
		return kept[i].lot < kept[j].lot
	})
	p.edges = kept
}
