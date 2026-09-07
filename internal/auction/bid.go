package auction

import (
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/GoldFridge/factorflow/internal/platform/apperr"
	"github.com/GoldFridge/factorflow/internal/platform/money"
	"github.com/GoldFridge/factorflow/internal/risk"
)

// Constraint names a rule that can stop a bid from taking a lot. The solver stores the
// first one that binds for every rejected bid, which is the explainability requirement:
// an investor is told which of their own limits refused the trade.
type Constraint string

// The constraints, in the order they are evaluated.
const (
	ConstraintCurrency            Constraint = "currency"
	ConstraintGrade               Constraint = "grade"
	ConstraintMaturity            Constraint = "maturity"
	ConstraintYield               Constraint = "yield"
	ConstraintMinimumLot          Constraint = "minimum_lot"
	ConstraintBudget              Constraint = "budget"
	ConstraintSupply              Constraint = "supply"
	ConstraintIssuerExposure      Constraint = "issuer_exposure"
	ConstraintDebtorExposure      Constraint = "debtor_exposure"
	ConstraintGradeExposure       Constraint = "grade_exposure"
	ConstraintNoRemainingCapacity Constraint = "no_remaining_capacity"
)

// String returns the wire representation.
func (c Constraint) String() string { return string(c) }

// NewBidParams carries an investor's constrained offer.
type NewBidParams struct {
	ID         uuid.UUID
	AuctionID  uuid.UUID
	InvestorID uuid.UUID

	Budget       money.Amount
	MinYield     money.Rate
	MaxGrade     risk.Grade
	MaxTenorDays int64
	MinimumLot   money.Amount

	MaxIssuerShare money.Rate
	MaxDebtorShare money.Rate
	MaxGradeShare  map[risk.Grade]money.Rate
}

// NewBid validates and builds an active bid.
func NewBid(p NewBidParams, now time.Time) (*Bid, error) {
	var violations []error

	if p.ID == uuid.Nil {
		violations = append(violations, apperr.Invalid("id", "must be a non-nil UUID"))
	}
	if p.AuctionID == uuid.Nil {
		violations = append(violations, apperr.Invalid("auction_id", "must be a non-nil UUID"))
	}
	if p.InvestorID == uuid.Nil {
		violations = append(violations, apperr.Invalid("investor_id", "must be a non-nil UUID"))
	}

	switch {
	case !p.Budget.IsValid():
		violations = append(violations, apperr.Invalid("budget", "must carry a supported currency"))
	case !p.Budget.IsPositive():
		violations = append(violations, apperr.Invalid("budget", "must be greater than zero"))
	}

	if p.MinYield.IsNegative() {
		violations = append(violations, apperr.Invalid("min_yield", "must not be negative, got %s", p.MinYield))
	}
	if !p.MaxGrade.IsValid() {
		violations = append(violations, apperr.Invalid("max_grade", "must be one of A..E, got %q", p.MaxGrade))
	}
	if p.MaxTenorDays <= 0 {
		violations = append(violations, apperr.Invalid("max_tenor_days", "must be greater than zero"))
	}

	switch {
	case !p.MinimumLot.IsValid():
		violations = append(violations, apperr.Invalid("minimum_lot", "must carry a supported currency"))
	case p.MinimumLot.IsNegative():
		violations = append(violations, apperr.Invalid("minimum_lot", "must not be negative"))
	case p.Budget.IsValid() && p.MinimumLot.Currency() != p.Budget.Currency():
		violations = append(violations, apperr.Invalid("minimum_lot",
			"must be in the same currency as the budget, got %s and %s", p.MinimumLot.Currency(), p.Budget.Currency()))
	}

	violations = append(violations, validateShare("max_issuer_share", p.MaxIssuerShare)...)
	violations = append(violations, validateShare("max_debtor_share", p.MaxDebtorShare)...)
	for grade, share := range p.MaxGradeShare {
		if !grade.IsValid() {
			violations = append(violations, apperr.Invalid("max_grade_share", "unknown grade %q", grade))
			continue
		}
		violations = append(violations, validateShare("max_grade_share."+grade.String(), share)...)
	}

	if err := errors.Join(violations...); err != nil {
		return nil, err
	}

	gradeShares := make(map[risk.Grade]money.Rate, len(p.MaxGradeShare))
	for grade, share := range p.MaxGradeShare {
		gradeShares[grade] = share
	}

	return &Bid{
		ID:             p.ID,
		AuctionID:      p.AuctionID,
		InvestorID:     p.InvestorID,
		Budget:         p.Budget,
		MinYield:       p.MinYield,
		MaxGrade:       p.MaxGrade,
		MaxTenorDays:   p.MaxTenorDays,
		MinimumLot:     p.MinimumLot,
		MaxIssuerShare: p.MaxIssuerShare,
		MaxDebtorShare: p.MaxDebtorShare,
		MaxGradeShare:  gradeShares,
		Status:         BidStatusActive,
		CreatedAt:      now.UTC(),
		Version:        1,
	}, nil
}

// validateShare rejects a concentration limit outside (0,1]. A zero share is rejected
// rather than treated as "no limit": an investor who means "no limit" omits the field, and
// silently reading zero as unlimited would be the dangerous interpretation.
func validateShare(field string, share money.Rate) []error {
	if share.IsZero() {
		return nil // omitted: uncapped
	}
	if share.IsNegative() || share.Cmp(money.OneRate()) > 0 {
		return []error{apperr.Invalid(field, "must be in (0,1], got %s", share)}
	}
	return nil
}

// Cancel withdraws the bid before clearing.
func (b *Bid) Cancel() error {
	if b.Status != BidStatusActive {
		return apperr.Conflictf("bid %s is %s and cannot be cancelled", b.ID, b.Status)
	}
	b.Status = BidStatusCancelled
	b.Version++
	return nil
}

// IssuerLimit is the most this bid may spend on any one issuer.
func (b *Bid) IssuerLimit() (money.Amount, error) { return b.shareLimit(b.MaxIssuerShare) }

// DebtorLimit is the most this bid may spend on any one debtor.
func (b *Bid) DebtorLimit() (money.Amount, error) { return b.shareLimit(b.MaxDebtorShare) }

// GradeLimit is the most this bid may spend on lots of one grade.
func (b *Bid) GradeLimit(grade risk.Grade) (money.Amount, error) {
	share, ok := b.MaxGradeShare[grade]
	if !ok {
		return b.Budget, nil
	}
	return b.shareLimit(share)
}

// shareLimit turns a share of the budget into an amount; an omitted share is the whole
// budget, which is the same as no limit.
func (b *Bid) shareLimit(share money.Rate) (money.Amount, error) {
	if share.IsZero() {
		return b.Budget, nil
	}
	return b.Budget.Mul(share)
}

// Feasible reports whether this bid may take any notional of the lot at all, and if not,
// which constraint bound first.
//
// It checks only the rules that depend on the pair, not on what has already been
// allocated: budget and exposure limits are enforced by the solver as it fills capacity.
// The evaluation order is fixed and documented so a rejection reason is reproducible.
func (b *Bid) Feasible(lot Lot) (bool, Constraint, error) {
	if b.Budget.Currency() != lot.Supply.Currency() {
		return false, ConstraintCurrency, nil
	}
	if !lot.Grade.AtMost(b.MaxGrade) {
		return false, ConstraintGrade, nil
	}
	if lot.TenorDays > b.MaxTenorDays {
		return false, ConstraintMaturity, nil
	}

	yield, err := lot.ImpliedYield()
	if err != nil {
		return false, "", err
	}
	if yield.Cmp(b.MinYield) < 0 {
		return false, ConstraintYield, nil
	}

	// The smallest allocation the investor accepts must fit in the lot and in the budget,
	// otherwise no feasible allocation exists on this edge at all.
	minimum := b.MinimumLot
	if minimum.IsPositive() {
		cmp, err := minimum.Cmp(lot.Supply)
		if err != nil {
			return false, "", err
		}
		if cmp > 0 {
			return false, ConstraintMinimumLot, nil
		}

		cost, err := lot.CostOf(minimum)
		if err != nil {
			return false, "", err
		}
		cmp, err = cost.Cmp(b.Budget)
		if err != nil {
			return false, "", err
		}
		if cmp > 0 {
			return false, ConstraintBudget, nil
		}
	}

	return true, "", nil
}
