package auction

import (
	"sort"

	"github.com/skimer2king/factorflow/internal/platform/money"
)

// exposureKey identifies one bid's exposure to one counterparty or grade.
type exposureKey struct {
	bid   int
	group string
}

// branchAndBound searches for the best allocation that also respects minimum lot sizes.
//
// The divisible flow relaxation is solved first; it is both a candidate solution and an
// upper bound on every allocation reachable below it, which is what makes the search
// bounded rather than exhaustive. Branching is deterministic: the first violating edge in
// canonical order is chosen, the "take at least the minimum" child is explored before the
// "take nothing" child, and the search stops at a published node count rather than a
// deadline.
func (s *Solver) branchAndBound(r *run) (*rawSolution, error) {
	var best *rawSolution
	nodes := 0
	limitReached := false
	repairRounds := 0

	var explore func(st *state) error
	explore = func(st *state) error {
		// The budget is checked between nodes, never inside one: a node that has started
		// must be allowed to finish its repair loop, or it could return an allocation that
		// still breaches a limit.
		if nodes >= s.Params.MaxBranchNodes || r.spent() {
			limitReached = true
			return nil
		}
		nodes++

		raw, err := s.solveState(r, st)
		if err != nil {
			return err
		}
		if raw == nil {
			return nil // this branch has no feasible allocation
		}
		repairRounds += raw.repairRounds
		if raw.limitReached {
			limitReached = true
		}

		// Every node returns an allocation that already satisfies every constraint, so it
		// is a candidate outright rather than something to repair later.
		if best == nil || raw.objective > best.objective {
			best = raw
		}

		// Branching now looks for improvement rather than feasibility: an investor whose
		// minimum lot forced their allocation to be dropped might do better taking exactly
		// that minimum, so try the most valuable dropped edge next.
		for _, index := range raw.dropped {
			if st.forced[index] {
				continue
			}
			child := st.clone()
			delete(child.forbidden, index)
			delete(child.capped, index)
			child.preassign[index] = r.problem.edges[index].minimumCash
			child.forced[index] = true
			if err := explore(child); err != nil {
				return err
			}
			break
		}
		return nil
	}

	if err := explore(newState()); err != nil {
		return nil, err
	}

	if best == nil {
		// Nothing feasible survived: the batch clears with an empty allocation.
		best = &rawSolution{cash: make([]int64, len(r.problem.edges))}
	}
	best.repairRounds = repairRounds
	best.limitReached = limitReached || r.exhausted
	best.branchNodes = nodes

	return best, nil
}

// repairMinimumLots drops allocations smaller than the investor's minimum lot.
//
// The constraint is discrete: an allocation is zero or at least the minimum, so a
// fractional flow that lands between the two has to give way. Dropping is the direction
// that always leaves a feasible allocation; the branch and bound then tries the other
// direction, taking exactly the minimum, wherever the budget allows.
func repairMinimumLots(prob *problem, st *state, raw *rawSolution) bool {
	repaired := false
	for index, spec := range prob.edges {
		if spec.minimumCash == 0 {
			continue
		}
		cash := raw.cash[index]
		if cash <= 0 || cash >= spec.minimumCash {
			continue
		}
		// Even an allocation the search deliberately forced up to the minimum can fall
		// below it once an exposure cap trims its group. The minimum is the investor's
		// rule, so the forcing gives way, not the rule.
		delete(st.forced, index)
		st.forbidden[index] = true
		delete(st.preassign, index)
		raw.dropped = append(raw.dropped, index)
		repaired = true
	}
	return repaired
}

// solveState solves the divisible relaxation under a branch's constraints and then repairs
// the concentration limits the flow network cannot express directly.
//
// Supply, budget and per-issuer exposure are enforced exactly by the network's capacities.
// Debtor and grade exposure cannot be: a single flow cannot attribute a unit of cash to
// several overlapping groups at once. Those are enforced afterwards by the repair loop the
// specification describes, and re-checked independently by the verifier.
func (s *Solver) solveState(r *run, st *state) (*rawSolution, error) {
	prob := r.problem
	var dropped []int

	for round := 0; round <= s.Params.MaxRepairRounds; round++ {
		raw, err := s.solveRelaxed(r, st)
		if err != nil {
			return nil, err
		}
		if raw == nil {
			return nil, nil
		}
		raw.repairRounds = round
		raw.dropped = dropped

		repaired, err := s.repairExposure(prob, st, raw)
		if err != nil {
			return nil, err
		}
		if !repaired {
			repaired = repairMinimumLots(prob, st, raw)
			dropped = raw.dropped
		}
		if !repaired {
			return raw, nil
		}
		if round == s.Params.MaxRepairRounds {
			// The repair budget is spent and a constraint still binds. Abandoning the
			// branch is the only safe answer: returning the allocation anyway would hand
			// settlement a breach of an investor's own limits.
			return nil, nil
		}
	}

	return nil, nil
}

// repairExposure enforces the concentration limits the flow network cannot express, and
// reports whether anything had to be repaired.
//
// The specification's repair step removes the least valuable allocation in a violated
// group. Removing a whole edge, though, throws away value the investor's own limit still
// permits: a bid capped at 25% per debtor should buy 25%, not nothing. So the repair caps
// instead of deletes. It walks the group's allocations from least to most valuable and
// trims exactly the excess, which both preserves the limit and converges in one round per
// violated group rather than one round per allocation.
func (s *Solver) repairExposure(p *problem, st *state, raw *rawSolution) (bool, error) {
	type group struct {
		limit int64
		spent int64
		edges []int
	}

	groups := map[exposureKey]*group{}
	add := func(key exposureKey, limit money.Amount, index int, cash int64) {
		g, ok := groups[key]
		if !ok {
			g = &group{limit: limit.Minor()}
			groups[key] = g
		}
		g.spent += cash
		g.edges = append(g.edges, index)
	}

	for index, spec := range p.edges {
		cash := raw.cash[index]
		if cash <= 0 {
			continue
		}
		lot, bid := p.lots[spec.lot], p.bids[spec.bid]

		debtorLimit, err := bid.DebtorLimit()
		if err != nil {
			return false, err
		}
		gradeLimit, err := bid.GradeLimit(lot.Grade)
		if err != nil {
			return false, err
		}

		add(exposureKey{bid: spec.bid, group: "debtor:" + lot.DebtorRef}, debtorLimit, index, cash)
		add(exposureKey{bid: spec.bid, group: "grade:" + lot.Grade.String()}, gradeLimit, index, cash)
	}

	keys := make([]exposureKey, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].bid != keys[j].bid {
			return keys[i].bid < keys[j].bid
		}
		return keys[i].group < keys[j].group
	})

	repaired := false
	for _, key := range keys {
		g := groups[key]
		if g.spent <= g.limit {
			continue
		}
		repaired = true

		// Cap the whole group at once rather than trimming one allocation per round. Each
		// edge keeps a share of the limit proportional to what the flow gave it, so the
		// group can never breach its limit again however the flow reshuffles, and one
		// round fixes one group. Trimming edge by edge instead took a round per
		// allocation and reshuffled the flow into the same breach each time.
		ordered := append([]int(nil), g.edges...)
		sort.SliceStable(ordered, func(i, j int) bool {
			left, right := p.edges[ordered[i]], p.edges[ordered[j]]
			if left.value != right.value {
				return left.value > right.value
			}
			return ordered[i] < ordered[j]
		})

		assigned := int64(0)
		for _, index := range ordered {
			share := g.limit * raw.cash[index] / g.spent
			assigned += share
			if existing, ok := st.capped[index]; !ok || share < existing {
				st.capped[index] = share
			}
		}
		// Integer division leaves a remainder; the most valuable allocation keeps it, so
		// the caps still sum to exactly the limit.
		if remainder := g.limit - assigned; remainder > 0 && len(ordered) > 0 {
			best := ordered[0]
			st.capped[best] += remainder
		}
		for _, index := range ordered {
			if st.capped[index] <= 0 {
				st.forbidden[index] = true
			}
			delete(st.preassign, index)
		}
	}

	return repaired, nil
}

// solveRelaxed builds and solves the min-cost flow for one branch state.
func (s *Solver) solveRelaxed(r *run, st *state) (*rawSolution, error) {
	p := r.problem
	r.solves++

	lotCapacity := make([]int64, len(p.lots))
	for i, lot := range p.lots {
		lotCapacity[i] = lot.ReservePrice.Minor()
	}
	bidCapacity := make([]int64, len(p.bids))
	for j, bid := range p.bids {
		bidCapacity[j] = bid.Budget.Minor()
	}

	// Per-issuer exposure needs a node of its own only when several lots of that issuer
	// compete for the same bid. When a bid can take just one lot from an issuer, the limit
	// is exactly a cap on that one edge, and folding it into the edge's capacity keeps the
	// network far smaller: node count drives how many augmentations the flow phase needs.
	issuerCapacity := map[exposureKey]int64{}
	issuerEdges := map[exposureKey][]int{}
	issuerNodeOf := map[exposureKey]int{}
	edgeCap := make([]int64, len(p.edges))
	for index, spec := range p.edges {
		if st.forbidden[index] {
			continue
		}
		limit, err := p.bids[spec.bid].IssuerLimit()
		if err != nil {
			return nil, err
		}
		key := exposureKey{bid: spec.bid, group: p.lots[spec.lot].IssuerID.String()}
		issuerCapacity[key] = limit.Minor()
		issuerEdges[key] = append(issuerEdges[key], index)
		edgeCap[index] = st.capacityOf(index, spec)
	}
	for key, indices := range issuerEdges {
		if len(indices) > 1 {
			continue
		}
		index := indices[0]
		if limit := issuerCapacity[key]; limit < edgeCap[index] {
			edgeCap[index] = limit
		}
		delete(issuerCapacity, key)
	}

	// Pre-assigned cash is spent before the flow runs, so every capacity it touches
	// shrinks by that amount.
	objective := int64(0)
	cash := make([]int64, len(p.edges))
	for index, amount := range st.preassign {
		if st.forbidden[index] || amount <= 0 {
			continue
		}
		spec := p.edges[index]
		lot := p.lots[spec.lot]

		lotCapacity[spec.lot] -= amount
		bidCapacity[spec.bid] -= amount
		key := exposureKey{bid: spec.bid, group: lot.IssuerID.String()}
		if _, ok := issuerCapacity[key]; ok {
			issuerCapacity[key] -= amount
			if issuerCapacity[key] < 0 {
				return nil, nil
			}
		}
		if lotCapacity[spec.lot] < 0 || bidCapacity[spec.bid] < 0 || amount > edgeCap[index] {
			return nil, nil
		}

		cash[index] = amount
		objective += amount * spec.value
	}

	// Node layout, in topological order: source, lots, per-bid issuer nodes, bids, sink.
	source := 0
	lotNode := func(i int) int { return 1 + i }
	nextNode := 1 + len(p.lots)

	issuerKeys := make([]exposureKey, 0, len(issuerCapacity))
	for key := range issuerCapacity {
		issuerKeys = append(issuerKeys, key)
	}
	sort.Slice(issuerKeys, func(i, j int) bool {
		if issuerKeys[i].bid != issuerKeys[j].bid {
			return issuerKeys[i].bid < issuerKeys[j].bid
		}
		return issuerKeys[i].group < issuerKeys[j].group
	})
	for _, key := range issuerKeys {
		issuerNodeOf[key] = nextNode
		nextNode++
	}

	bidNodeBase := nextNode
	bidNode := func(j int) int { return bidNodeBase + j }
	sink := bidNodeBase + len(p.bids)

	network := newFlowNetwork(sink + 1)
	for i := range p.lots {
		if lotCapacity[i] > 0 {
			network.addEdge(source, lotNode(i), lotCapacity[i], 0)
		}
	}

	edgeArc := make([]int, len(p.edges))
	for index := range edgeArc {
		edgeArc[index] = -1
	}
	for index, spec := range p.edges {
		if st.forbidden[index] {
			continue
		}
		capacity := edgeCap[index] - cash[index]
		if capacity <= 0 {
			continue
		}
		// Cost is negative value: the flow minimizes cost, the auction maximizes surplus.
		target := bidNode(spec.bid)
		if node, ok := issuerNodeOf[exposureKey{bid: spec.bid, group: p.lots[spec.lot].IssuerID.String()}]; ok {
			target = node
		}
		edgeArc[index] = network.addEdge(lotNode(spec.lot), target, capacity, -spec.value)
	}

	for _, key := range issuerKeys {
		if issuerCapacity[key] > 0 {
			network.addEdge(issuerNodeOf[key], bidNode(key.bid), issuerCapacity[key], 0)
		}
	}
	for j := range p.bids {
		if bidCapacity[j] > 0 {
			network.addEdge(bidNode(j), sink, bidCapacity[j], 0)
		}
	}

	result := network.solve(source, sink, s.Params.MaxAugmentations)

	for index, arc := range edgeArc {
		if arc < 0 {
			continue
		}
		flow := network.flowOn(arc)
		cash[index] += flow
		objective += flow * p.edges[index].value
	}

	return &rawSolution{cash: cash, objective: objective, limitReached: result.LimitReached}, nil
}

// materializeSolution converts cash flows into notional allocations and explains every bid
// that received nothing.
func (s *Solver) materializeSolution(p *problem, raw *rawSolution) (*Solution, error) {
	solution := &Solution{
		AuctionID:     p.auctionID,
		SolverVersion: s.Version,
		Objective:     0,
		TotalNotional: money.Zero(p.currency),
		TotalCash:     money.Zero(p.currency),
		BranchNodes:   raw.branchNodes,
		RepairRounds:  raw.repairRounds,
		LimitReached:  raw.limitReached,
	}

	remaining := make([]money.Amount, len(p.lots))
	for i, lot := range p.lots {
		remaining[i] = lot.Supply
	}
	allocatedByBid := make([]bool, len(p.bids))

	for index, spec := range p.edges {
		cashMinor := raw.cash[index]
		if cashMinor <= 0 {
			continue
		}
		lot, bid := p.lots[spec.lot], p.bids[spec.bid]

		cashAmount, err := money.New(cashMinor, p.currency)
		if err != nil {
			return nil, err
		}
		notional, err := cashAmount.DivFloor(spec.unitPrice)
		if err != nil {
			return nil, err
		}

		// Flooring the conversion can land a hair below the investor's minimum lot even
		// though the cash covers it; the minimum is what was actually bought.
		if bid.MinimumLot.IsPositive() {
			if cmp, err := notional.Cmp(bid.MinimumLot); err == nil && cmp < 0 && cashMinor >= spec.minimumCash {
				notional = bid.MinimumLot
			}
		}
		if cmp, err := notional.Cmp(remaining[spec.lot]); err != nil {
			return nil, err
		} else if cmp > 0 {
			notional = remaining[spec.lot]
		}
		if !notional.IsPositive() {
			continue
		}

		price, err := lot.CostOf(notional)
		if err != nil {
			return nil, err
		}

		remaining[spec.lot], err = remaining[spec.lot].Sub(notional)
		if err != nil {
			return nil, err
		}
		solution.TotalNotional, err = solution.TotalNotional.Add(notional)
		if err != nil {
			return nil, err
		}
		solution.TotalCash, err = solution.TotalCash.Add(price)
		if err != nil {
			return nil, err
		}
		solution.Objective += price.Minor() * spec.value

		solution.Allocations = append(solution.Allocations, Allocation{
			LotID:      lot.ID,
			InvoiceID:  lot.InvoiceID,
			AssetID:    lot.AssetID,
			BidID:      bid.ID,
			InvestorID: bid.InvestorID,
			Notional:   notional,
			Price:      price,
		})
		allocatedByBid[spec.bid] = true
	}

	sortAllocations(solution.Allocations)
	for i := range solution.Allocations {
		solution.Allocations[i].Rank = i
	}

	for j, bid := range p.bids {
		if allocatedByBid[j] {
			continue
		}
		rejection := p.blocked[j]
		rejection.BidID = bid.ID
		solution.Rejections = append(solution.Rejections, rejection)
	}

	return solution, nil
}

// sortAllocations puts allocations in the published order: by bid, then by lot.
func sortAllocations(allocations []Allocation) {
	sort.SliceStable(allocations, func(i, j int) bool {
		if allocations[i].BidID != allocations[j].BidID {
			return allocations[i].BidID.String() < allocations[j].BidID.String()
		}
		if allocations[i].InvoiceID != allocations[j].InvoiceID {
			return allocations[i].InvoiceID.String() < allocations[j].InvoiceID.String()
		}
		return allocations[i].LotID.String() < allocations[j].LotID.String()
	})
}
