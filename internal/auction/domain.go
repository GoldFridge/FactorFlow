// Package auction holds the batch auction: the lots a cleared invoice offers, the
// constrained bids investors place against them, and the deterministic clearing that
// matches the two.
//
// A note on scope, because the specification can be read two ways. An auction here is one
// batch that may carry several lots, one per tokenized asset, and an asset appears in at
// most one open auction. That is what makes the cross-lot constraints in the specification
// meaningful: an investor's exposure limit per issuer, debtor and grade only binds if the
// solver can see more than one lot at a time.
package auction

import (
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/skimer2king/factorflow/internal/platform/apperr"
	"github.com/skimer2king/factorflow/internal/platform/money"
	"github.com/skimer2king/factorflow/internal/risk"
)

// Lot is one tokenized receivable offered in an auction.
//
// Supply is notional in face-value terms; ReservePrice is what the whole supply costs at
// the price the risk model published. An allocation of x notional therefore costs
// x * ReservePrice / Supply.
type Lot struct {
	ID        uuid.UUID
	InvoiceID uuid.UUID
	AssetID   uuid.UUID
	IssuerID  uuid.UUID
	DebtorRef string

	Supply       money.Amount
	ReservePrice money.Amount
	Grade        risk.Grade
	TenorDays    int64
}

// Bid is one investor's constrained offer over the whole batch.
type Bid struct {
	ID         uuid.UUID
	AuctionID  uuid.UUID
	InvestorID uuid.UUID

	// Budget is the cash the investor will spend across the batch.
	Budget money.Amount
	// MinYield is the annualized return the investor requires. A lot priced below it is
	// not eligible for this bid.
	MinYield money.Rate
	// MaxGrade is the riskiest grade the investor accepts.
	MaxGrade risk.Grade
	// MaxTenorDays is the longest maturity the investor accepts.
	MaxTenorDays int64
	// MinimumLot is the smallest allocation the investor will take. An allocation is
	// either zero or at least this much notional.
	MinimumLot money.Amount

	// MaxIssuerShare and MaxDebtorShare cap the share of the budget that may be spent on
	// any one issuer or debtor.
	MaxIssuerShare money.Rate
	MaxDebtorShare money.Rate
	// MaxGradeShare caps the share of the budget spent on lots of a given grade. Grades
	// absent from the map are uncapped.
	MaxGradeShare map[risk.Grade]money.Rate

	Status    BidStatus
	CreatedAt time.Time
	Version   int64
}

// Auction is the batch aggregate.
type Auction struct {
	ID       uuid.UUID
	IssuerID uuid.UUID

	Lots []Lot

	OpensAt  time.Time
	ClosesAt time.Time

	Status Status
	// SolverVersion records which solver produced the allocation.
	SolverVersion string
	// CertificateHash is the allocation certificate of the cleared batch.
	CertificateHash string
	// Reason carries the cause of a CANCELLED or FAILED auction.
	Reason string

	Version   int64
	CreatedAt time.Time
	UpdatedAt time.Time
}

// NewAuctionParams carries the facts needed to open a batch.
type NewAuctionParams struct {
	ID       uuid.UUID
	IssuerID uuid.UUID
	Lots     []Lot
	OpensAt  time.Time
	ClosesAt time.Time
}

// NewAuction creates a DRAFT auction, rejecting a batch that breaks an invariant.
func NewAuction(p NewAuctionParams, now time.Time) (*Auction, error) {
	var violations []error

	if p.ID == uuid.Nil {
		violations = append(violations, apperr.Invalid("id", "must be a non-nil UUID"))
	}
	if p.IssuerID == uuid.Nil {
		violations = append(violations, apperr.Invalid("issuer_id", "must be a non-nil UUID"))
	}
	if len(p.Lots) == 0 {
		violations = append(violations, apperr.Invalid("lots", "must contain at least one lot"))
	}
	if !p.ClosesAt.After(p.OpensAt) {
		violations = append(violations, apperr.Invalid("closes_at", "must be after opens_at"))
	}

	seenAssets := make(map[uuid.UUID]struct{}, len(p.Lots))
	currency := money.Currency("")
	for i, lot := range p.Lots {
		if err := lot.Validate(); err != nil {
			violations = append(violations, err)
			continue
		}
		if _, duplicate := seenAssets[lot.AssetID]; duplicate {
			violations = append(violations, apperr.Invalid("lots",
				"asset %s appears twice; an asset belongs to at most one open auction", lot.AssetID))
		}
		seenAssets[lot.AssetID] = struct{}{}

		if i == 0 {
			currency = lot.Supply.Currency()
		} else if lot.Supply.Currency() != currency {
			violations = append(violations, apperr.Invalid("lots",
				"batch mixes currencies %s and %s", currency, lot.Supply.Currency()))
		}
	}

	if err := errors.Join(violations...); err != nil {
		return nil, err
	}

	return &Auction{
		ID:        p.ID,
		IssuerID:  p.IssuerID,
		Lots:      append([]Lot(nil), p.Lots...),
		OpensAt:   p.OpensAt.UTC(),
		ClosesAt:  p.ClosesAt.UTC(),
		Status:    StatusDraft,
		Version:   1,
		CreatedAt: now.UTC(),
		UpdatedAt: now.UTC(),
	}, nil
}

// Currency returns the currency of the batch. It panics on an auction with no lots, which
// NewAuction refuses to create.
func (a *Auction) Currency() money.Currency { return a.Lots[0].Supply.Currency() }

// TotalSupply is the notional offered across the batch.
func (a *Auction) TotalSupply() (money.Amount, error) {
	amounts := make([]money.Amount, 0, len(a.Lots))
	for _, lot := range a.Lots {
		amounts = append(amounts, lot.Supply)
	}
	return money.Sum(amounts...)
}

// Lot returns the lot with the given id.
func (a *Auction) Lot(id uuid.UUID) (Lot, bool) {
	for _, lot := range a.Lots {
		if lot.ID == id {
			return lot, true
		}
	}
	return Lot{}, false
}

// IsAcceptingBids reports whether a bid may still be placed.
func (a *Auction) IsAcceptingBids(now time.Time) bool {
	return a.Status == StatusOpen && !now.Before(a.OpensAt) && now.Before(a.ClosesAt)
}

// Validate checks the facts of a lot.
func (l Lot) Validate() error {
	var violations []error

	if l.ID == uuid.Nil {
		violations = append(violations, apperr.Invalid("lot.id", "must be a non-nil UUID"))
	}
	if l.InvoiceID == uuid.Nil {
		violations = append(violations, apperr.Invalid("lot.invoice_id", "must be a non-nil UUID"))
	}
	if l.AssetID == uuid.Nil {
		violations = append(violations, apperr.Invalid("lot.asset_id", "must be a non-nil UUID"))
	}
	if l.IssuerID == uuid.Nil {
		violations = append(violations, apperr.Invalid("lot.issuer_id", "must be a non-nil UUID"))
	}
	if strings.TrimSpace(l.DebtorRef) == "" {
		violations = append(violations, apperr.Invalid("lot.debtor_ref", "must not be empty"))
	}
	if !l.Grade.IsValid() {
		violations = append(violations, apperr.Invalid("lot.grade", "must be one of A..E, got %q", l.Grade))
	}
	if l.TenorDays <= 0 {
		violations = append(violations, apperr.Invalid("lot.tenor_days", "must be greater than zero"))
	}

	switch {
	case !l.Supply.IsValid():
		violations = append(violations, apperr.Invalid("lot.supply", "must carry a supported currency"))
	case !l.Supply.IsPositive():
		violations = append(violations, apperr.Invalid("lot.supply", "must be greater than zero"))
	}

	switch {
	case !l.ReservePrice.IsValid():
		violations = append(violations, apperr.Invalid("lot.reserve_price", "must carry a supported currency"))
	case !l.ReservePrice.IsPositive():
		violations = append(violations, apperr.Invalid("lot.reserve_price", "must be greater than zero"))
	case l.Supply.IsValid() && l.ReservePrice.Currency() != l.Supply.Currency():
		violations = append(violations, apperr.Invalid("lot.reserve_price",
			"must be in the same currency as supply, got %s and %s", l.ReservePrice.Currency(), l.Supply.Currency()))
	}

	if err := errors.Join(violations...); err != nil {
		return err
	}

	// A receivable sold for more than its face value would hand the investor a negative
	// yield; the risk model never produces such a price, and the auction refuses it.
	if cmp, err := l.ReservePrice.Cmp(l.Supply); err == nil && cmp >= 0 {
		return apperr.Invalid("lot.reserve_price",
			"must be below face supply %s to offer a positive yield, got %s", l.Supply, l.ReservePrice)
	}
	return nil
}

// UnitPrice is the price of one unit of notional, as a rate below 1.
func (l Lot) UnitPrice() (money.Rate, error) { return l.ReservePrice.RateAgainst(l.Supply) }

// ImpliedYield is the annualized return an investor earns buying the whole lot at its
// reserve price:
//
//	(face - price) / price * day_count / tenor_days
func (l Lot) ImpliedYield() (money.Rate, error) {
	discount, err := l.Supply.Sub(l.ReservePrice)
	if err != nil {
		return money.Rate{}, err
	}
	periodReturn, err := discount.RateAgainst(l.ReservePrice)
	if err != nil {
		return money.Rate{}, err
	}
	annualization, err := money.RateFromFraction(dayCount, l.TenorDays)
	if err != nil {
		return money.Rate{}, err
	}
	return periodReturn.Mul(annualization).Quantize(rateScale), nil
}

// CostOf is what a notional allocation costs at the lot's unit price, rounded half to even.
func (l Lot) CostOf(notional money.Amount) (money.Amount, error) {
	unitPrice, err := l.UnitPrice()
	if err != nil {
		return money.Amount{}, err
	}
	return notional.Mul(unitPrice)
}

const (
	// dayCount is the annualization basis of an implied yield.
	dayCount int64 = 365
	// rateScale is the precision of derived rates, matching the risk model.
	rateScale int32 = 6
)
