// Package redemption is the end of a receivable's life: the debtor pays, and the money is
// divided among the parties that hold it.
//
// The division is the whole reason this is a module rather than a few lines in a handler.
// A receivable is sold in parts, so what arrives has to be split in proportion to what
// each party holds — and split exactly, in minor units, with no unit lost to rounding and
// none invented. That rule is arithmetic, it is testable on its own, and it must produce
// the same answer every time it is asked, because two parties will read the result and
// only one of them is the platform.
package redemption

import (
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/GoldFridge/factorflow/internal/platform/apperr"
	"github.com/GoldFridge/factorflow/internal/platform/money"
)

// MaxReferenceLen bounds the debtor's payment reference.
const MaxReferenceLen = 128

// Holder is a party owed part of what the debtor pays, and how much of the receivable it
// holds. Notional is face value, not what was paid for it: what the debtor owes is the
// face, and that is what is being divided.
type Holder struct {
	PartyID  uuid.UUID
	Notional money.Amount
}

// Share is one holder's part of a repayment.
type Share struct {
	PartyID  uuid.UUID
	Notional money.Amount
	Amount   money.Amount
}

// Repayment is the debtor's payment against one receivable, together with the division of
// it that the payment implies.
//
// The shares are computed here rather than supplied, so a repayment cannot exist whose
// parts fail to add up to the whole.
type Repayment struct {
	ID        uuid.UUID
	InvoiceID uuid.UUID

	// Face is what was owed and Amount is what arrived. They are separate because the
	// difference is the whole question at maturity.
	Face   money.Amount
	Amount money.Amount

	// Reference is the debtor's own payment reference, which is how a person reconciles
	// this record against a bank statement.
	Reference  string
	ReceivedAt time.Time
	// RecordedBy is the organization that confirmed the money arrived.
	RecordedBy uuid.UUID

	Shares    []Share
	CreatedAt time.Time
}

// NewParams are the facts of a payment that arrived.
type NewParams struct {
	ID         uuid.UUID
	InvoiceID  uuid.UUID
	Face       money.Amount
	Amount     money.Amount
	Reference  string
	ReceivedAt time.Time
	RecordedBy uuid.UUID
	Holders    []Holder
}

// New records a payment and divides it among the holders.
func New(p NewParams, now time.Time) (*Repayment, error) {
	var violations []error

	if p.ID == uuid.Nil {
		violations = append(violations, apperr.Invalid("id", "must be a non-nil UUID"))
	}
	if p.InvoiceID == uuid.Nil {
		violations = append(violations, apperr.Invalid("invoice_id", "must be a non-nil UUID"))
	}
	if p.RecordedBy == uuid.Nil {
		violations = append(violations, apperr.Invalid("recorded_by", "must be a non-nil UUID"))
	}

	reference := strings.TrimSpace(p.Reference)
	switch {
	case reference == "":
		violations = append(violations, apperr.Invalid("reference", "must not be empty"))
	case len(reference) > MaxReferenceLen:
		violations = append(violations, apperr.Invalid("reference",
			"must be at most %d characters", MaxReferenceLen))
	}

	switch {
	case !p.Face.IsValid() || !p.Face.IsPositive():
		violations = append(violations, apperr.Invalid("face", "must be a positive amount"))
	case !p.Amount.IsValid() || !p.Amount.IsPositive():
		violations = append(violations, apperr.Invalid("amount", "must be a positive amount"))
	case p.Amount.Currency() != p.Face.Currency():
		violations = append(violations, apperr.Invalid("amount",
			"must be in %s, the currency of the receivable", p.Face.Currency()))
	default:
		if more, err := p.Amount.Cmp(p.Face); err == nil && more > 0 {
			// Refused rather than truncated: a debtor who paid more than is owed has done
			// something this system has no rule for, and inventing one here would quietly
			// give the surplus to whoever happens to round up.
			violations = append(violations, apperr.Invalid("amount",
				"must not exceed the %s owed", p.Face))
		}
	}

	if p.ReceivedAt.IsZero() {
		violations = append(violations, apperr.Invalid("received_at", "must be set"))
	} else if p.ReceivedAt.After(now) {
		violations = append(violations, apperr.Invalid("received_at", "must not be in the future"))
	}

	if err := errors.Join(violations...); err != nil {
		return nil, err
	}

	shares, err := Distribute(p.Amount, p.Holders)
	if err != nil {
		return nil, err
	}

	return &Repayment{
		ID:         p.ID,
		InvoiceID:  p.InvoiceID,
		Face:       p.Face,
		Amount:     p.Amount,
		Reference:  reference,
		ReceivedAt: p.ReceivedAt.UTC(),
		RecordedBy: p.RecordedBy,
		Shares:     shares,
		CreatedAt:  now.UTC(),
	}, nil
}

// IsShortfall reports whether the debtor paid less than was owed.
//
// A short payment is still a payment and is still divided; what it is not is a receivable
// that came good, and the caller has to say so somewhere a holder can see it.
func (r *Repayment) IsShortfall() bool {
	less, err := r.Amount.Cmp(r.Face)
	return err == nil && less < 0
}

// Shortfall is what did not arrive, zero when the receivable was paid in full.
func (r *Repayment) Shortfall() money.Amount {
	if !r.IsShortfall() {
		return money.Zero(r.Face.Currency())
	}
	missing, err := r.Face.Sub(r.Amount)
	if err != nil {
		return money.Zero(r.Face.Currency())
	}
	return missing
}

// ShareOf returns what one party is owed from this repayment.
func (r *Repayment) ShareOf(partyID uuid.UUID) (Share, bool) {
	for _, share := range r.Shares {
		if share.PartyID == partyID {
			return share, true
		}
	}
	return Share{}, false
}

// Holds reports whether a party has a share in this repayment, which is what makes it
// entitled to read the record at all.
func (r *Repayment) Holds(partyID uuid.UUID) bool {
	_, ok := r.ShareOf(partyID)
	return ok
}
