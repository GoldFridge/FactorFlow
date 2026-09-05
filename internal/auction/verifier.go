package auction

import (
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/skimer2king/factorflow/internal/platform/money"
	"github.com/skimer2king/factorflow/internal/risk"
)

// ErrVerification reports an allocation the independent verifier refused. The caller must
// discard the solution and mark the clearing failed rather than settle it.
var ErrVerification = errors.New("auction: allocation failed independent verification")

// Verify recomputes every constraint of a solution from the original auction and bids.
//
// It deliberately shares no arithmetic with the solver: the sums, the value of each
// allocation and the eligibility of each pair are all computed again here from the domain
// objects. A solver bug that quietly overspends a budget or hands an investor a grade they
// refused therefore fails here rather than reaching settlement.
func Verify(a *Auction, bids []*Bid, s *Solution, params SolverParams) error {
	if a == nil || s == nil {
		return fmt.Errorf("%w: missing auction or solution", ErrVerification)
	}
	if s.AuctionID != a.ID {
		return fmt.Errorf("%w: solution is for auction %s, not %s", ErrVerification, s.AuctionID, a.ID)
	}

	lotsByID := make(map[uuid.UUID]Lot, len(a.Lots))
	for _, lot := range a.Lots {
		lotsByID[lot.ID] = lot
	}
	bidsByID := make(map[uuid.UUID]*Bid, len(bids))
	for _, bid := range bids {
		if bid != nil && bid.Status == BidStatusActive {
			bidsByID[bid.ID] = bid
		}
	}

	currency := a.Currency()
	notionalPerLot := map[uuid.UUID]money.Amount{}
	cashPerBid := map[uuid.UUID]money.Amount{}
	cashPerIssuer := map[string]money.Amount{}
	cashPerDebtor := map[string]money.Amount{}
	cashPerGrade := map[string]money.Amount{}
	seenPairs := map[string]bool{}

	totalNotional := money.Zero(currency)
	totalCash := money.Zero(currency)
	objective := int64(0)

	for i, allocation := range s.Allocations {
		if allocation.Rank != i {
			return fmt.Errorf("%w: allocation %d carries rank %d", ErrVerification, i, allocation.Rank)
		}

		lot, ok := lotsByID[allocation.LotID]
		if !ok {
			return fmt.Errorf("%w: allocation references unknown lot %s", ErrVerification, allocation.LotID)
		}
		bid, ok := bidsByID[allocation.BidID]
		if !ok {
			return fmt.Errorf("%w: allocation references unknown or inactive bid %s", ErrVerification, allocation.BidID)
		}

		pair := allocation.LotID.String() + "/" + allocation.BidID.String()
		if seenPairs[pair] {
			return fmt.Errorf("%w: lot %s is allocated to bid %s twice", ErrVerification, allocation.LotID, allocation.BidID)
		}
		seenPairs[pair] = true

		if allocation.InvoiceID != lot.InvoiceID || allocation.AssetID != lot.AssetID {
			return fmt.Errorf("%w: allocation %d does not match lot %s", ErrVerification, i, lot.ID)
		}
		if allocation.InvestorID != bid.InvestorID {
			return fmt.Errorf("%w: allocation %d does not match bid %s", ErrVerification, i, bid.ID)
		}
		if !allocation.Notional.IsPositive() {
			return fmt.Errorf("%w: allocation %d has no notional", ErrVerification, i)
		}

		if err := verifyEligibility(lot, bid); err != nil {
			return err
		}
		if err := verifyMinimumLot(lot, bid, allocation); err != nil {
			return err
		}

		expectedPrice, err := lot.CostOf(allocation.Notional)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrVerification, err)
		}
		if !expectedPrice.Equal(allocation.Price) {
			return fmt.Errorf("%w: allocation %d prices %s at %s, expected %s",
				ErrVerification, i, allocation.Notional, allocation.Price, expectedPrice)
		}

		if notionalPerLot[lot.ID], err = addTo(notionalPerLot[lot.ID], allocation.Notional, currency); err != nil {
			return err
		}
		if cashPerBid[bid.ID], err = addTo(cashPerBid[bid.ID], allocation.Price, currency); err != nil {
			return err
		}
		issuerKey := bid.ID.String() + "/" + lot.IssuerID.String()
		if cashPerIssuer[issuerKey], err = addTo(cashPerIssuer[issuerKey], allocation.Price, currency); err != nil {
			return err
		}
		debtorKey := bid.ID.String() + "/" + lot.DebtorRef
		if cashPerDebtor[debtorKey], err = addTo(cashPerDebtor[debtorKey], allocation.Price, currency); err != nil {
			return err
		}
		gradeKey := bid.ID.String() + "/" + lot.Grade.String()
		if cashPerGrade[gradeKey], err = addTo(cashPerGrade[gradeKey], allocation.Price, currency); err != nil {
			return err
		}

		if totalNotional, err = totalNotional.Add(allocation.Notional); err != nil {
			return fmt.Errorf("%w: %v", ErrVerification, err)
		}
		if totalCash, err = totalCash.Add(allocation.Price); err != nil {
			return fmt.Errorf("%w: %v", ErrVerification, err)
		}

		value, err := edgeValuePoints(lot, bid, params)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrVerification, err)
		}
		objective += allocation.Price.Minor() * value
	}

	for lotID, allocated := range notionalPerLot {
		lot := lotsByID[lotID]
		if cmp, err := allocated.Cmp(lot.Supply); err != nil {
			return fmt.Errorf("%w: %v", ErrVerification, err)
		} else if cmp > 0 {
			return fmt.Errorf("%w: lot %s allocates %s of %s supply", ErrVerification, lotID, allocated, lot.Supply)
		}
	}

	for bidID, spent := range cashPerBid {
		bid := bidsByID[bidID]
		if cmp, err := spent.Cmp(bid.Budget); err != nil {
			return fmt.Errorf("%w: %v", ErrVerification, err)
		} else if cmp > 0 {
			return fmt.Errorf("%w: bid %s spends %s of a %s budget", ErrVerification, bidID, spent, bid.Budget)
		}
	}

	if err := verifyExposure(cashPerIssuer, bidsByID, ConstraintIssuerExposure, func(bid *Bid, _ string) (money.Amount, error) {
		return bid.IssuerLimit()
	}); err != nil {
		return err
	}
	if err := verifyExposure(cashPerDebtor, bidsByID, ConstraintDebtorExposure, func(bid *Bid, _ string) (money.Amount, error) {
		return bid.DebtorLimit()
	}); err != nil {
		return err
	}
	if err := verifyExposure(cashPerGrade, bidsByID, ConstraintGradeExposure, func(bid *Bid, group string) (money.Amount, error) {
		grade, err := risk.ParseGrade(group)
		if err != nil {
			return money.Amount{}, err
		}
		return bid.GradeLimit(grade)
	}); err != nil {
		return err
	}

	if !totalNotional.Equal(s.TotalNotional) {
		return fmt.Errorf("%w: notional total is %s, solution claims %s", ErrVerification, totalNotional, s.TotalNotional)
	}
	if !totalCash.Equal(s.TotalCash) {
		return fmt.Errorf("%w: cash total is %s, solution claims %s", ErrVerification, totalCash, s.TotalCash)
	}
	if objective != s.Objective {
		return fmt.Errorf("%w: objective is %d, solution claims %d", ErrVerification, objective, s.Objective)
	}

	for _, rejection := range s.Rejections {
		if _, allocated := cashPerBid[rejection.BidID]; allocated {
			return fmt.Errorf("%w: bid %s is both allocated and rejected", ErrVerification, rejection.BidID)
		}
		if rejection.Constraint == "" {
			return fmt.Errorf("%w: bid %s was rejected without a reason", ErrVerification, rejection.BidID)
		}
	}

	return nil
}

// verifyEligibility recomputes the pair rules rather than reusing the solver's feasibility
// check, so an error in that check cannot approve itself.
func verifyEligibility(lot Lot, bid *Bid) error {
	if lot.Supply.Currency() != bid.Budget.Currency() {
		return fmt.Errorf("%w: lot %s is in %s, bid %s in %s",
			ErrVerification, lot.ID, lot.Supply.Currency(), bid.ID, bid.Budget.Currency())
	}
	if !lot.Grade.AtMost(bid.MaxGrade) {
		return fmt.Errorf("%w: lot %s is grade %s, bid %s accepts at most %s",
			ErrVerification, lot.ID, lot.Grade, bid.ID, bid.MaxGrade)
	}
	if lot.TenorDays > bid.MaxTenorDays {
		return fmt.Errorf("%w: lot %s matures in %d days, bid %s accepts at most %d",
			ErrVerification, lot.ID, lot.TenorDays, bid.ID, bid.MaxTenorDays)
	}

	yield, err := lot.ImpliedYield()
	if err != nil {
		return fmt.Errorf("%w: %v", ErrVerification, err)
	}
	if yield.Cmp(bid.MinYield) < 0 {
		return fmt.Errorf("%w: lot %s yields %s, bid %s requires %s",
			ErrVerification, lot.ID, yield, bid.ID, bid.MinYield)
	}
	return nil
}

func verifyMinimumLot(lot Lot, bid *Bid, allocation Allocation) error {
	if !bid.MinimumLot.IsPositive() {
		return nil
	}
	cmp, err := allocation.Notional.Cmp(bid.MinimumLot)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrVerification, err)
	}
	if cmp < 0 {
		return fmt.Errorf("%w: lot %s allocates %s to bid %s, below its %s minimum",
			ErrVerification, lot.ID, allocation.Notional, bid.ID, bid.MinimumLot)
	}
	return nil
}

// verifyExposure checks one concentration dimension. Keys are "bidID/group".
func verifyExposure(
	spent map[string]money.Amount,
	bidsByID map[uuid.UUID]*Bid,
	constraint Constraint,
	limitOf func(*Bid, string) (money.Amount, error),
) error {
	for key, amount := range spent {
		bidID, group, err := splitExposureKey(key)
		if err != nil {
			return err
		}
		bid, ok := bidsByID[bidID]
		if !ok {
			return fmt.Errorf("%w: exposure recorded for unknown bid %s", ErrVerification, bidID)
		}

		limit, err := limitOf(bid, group)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrVerification, err)
		}
		cmp, err := amount.Cmp(limit)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrVerification, err)
		}
		if cmp > 0 {
			return fmt.Errorf("%w: bid %s breaches %s on %q: %s of a %s limit",
				ErrVerification, bidID, constraint, group, amount, limit)
		}
	}
	return nil
}

func splitExposureKey(key string) (uuid.UUID, string, error) {
	const uuidLen = 36
	if len(key) < uuidLen+1 {
		return uuid.Nil, "", fmt.Errorf("%w: malformed exposure key %q", ErrVerification, key)
	}
	id, err := uuid.Parse(key[:uuidLen])
	if err != nil {
		return uuid.Nil, "", fmt.Errorf("%w: malformed exposure key %q", ErrVerification, key)
	}
	return id, key[uuidLen+1:], nil
}

func addTo(current, delta money.Amount, currency money.Currency) (money.Amount, error) {
	if !current.IsValid() {
		current = money.Zero(currency)
	}
	sum, err := current.Add(delta)
	if err != nil {
		return money.Amount{}, fmt.Errorf("%w: %v", ErrVerification, err)
	}
	return sum, nil
}

// edgeValuePoints recomputes the value of one allocation in the same units the objective
// is expressed in.
func edgeValuePoints(lot Lot, bid *Bid, params SolverParams) (int64, error) {
	yield, err := lot.ImpliedYield()
	if err != nil {
		return 0, err
	}
	value := params.SurplusWeight.Mul(yield.Sub(bid.MinYield)).
		Add(params.FundingWeight).
		Sub(params.RiskPenalty.MulInt(int64(lot.Grade.Rank())))

	points := value.Mul(money.RateFromInt(valueScale)).Decimal().RoundBank(0).IntPart()
	if points < 0 {
		points = 0
	}
	return points, nil
}
