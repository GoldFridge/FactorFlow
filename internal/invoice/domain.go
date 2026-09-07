// Package invoice owns the receivable aggregate: its facts, its invariants and the state
// machine that moves it from a draft to a settled, matured or defaulted asset.
//
// The aggregate is storage- and transport-agnostic. It never talks to PostgreSQL, a chain
// adapter or an HTTP handler; those live in the module's ports and adapters, and they call
// in through the commands defined here. Every command validates the current state before
// mutating, and bumps Version so the repository can detect a concurrent writer.
package invoice

import (
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/GoldFridge/factorflow/internal/platform/apperr"
	"github.com/GoldFridge/factorflow/internal/platform/money"
)

// Limits on invoice facts. They bound the synthetic demo dataset and keep the risk model
// inside the range its coefficients were calibrated on.
const (
	// MaxDebtorRefLen bounds the debtor reference stored alongside the invoice.
	MaxDebtorRefLen = 128
	// MaxNumberLen bounds the issuer's invoice number.
	MaxNumberLen = 64
	// MinTenorDays is the shortest financeable tenor.
	MinTenorDays = 1
	// MaxTenorDays is the longest financeable tenor; the specification targets 30-120 day
	// receivables and refuses anything a short-duration market would not price.
	MaxTenorDays = 365
)

// Invoice is the receivable aggregate.
type Invoice struct {
	ID        uuid.UUID
	IssuerID  uuid.UUID
	DebtorRef string
	Number    string
	Face      money.Amount
	IssuedAt  time.Time
	DueAt     time.Time

	Status Status
	// FailedFrom records the stage that produced a FAILED status, so a manual retry
	// resumes the right step instead of guessing.
	FailedFrom Status
	// Reason carries the human-readable cause of a REJECTED or FAILED status. It never
	// holds document content: the privacy rule forbids plaintext outside the TEE.
	Reason string

	// AssessmentID is the immutable risk assessment the approval was based on.
	AssessmentID uuid.UUID
	// AssetID is the tokenized asset issued for this invoice.
	AssetID uuid.UUID

	Version   int64
	CreatedAt time.Time
	UpdatedAt time.Time
}

// NewParams carries the facts an issuer supplies when creating a draft.
type NewParams struct {
	ID        uuid.UUID
	IssuerID  uuid.UUID
	DebtorRef string
	Number    string
	Face      money.Amount
	IssuedAt  time.Time
	DueAt     time.Time
}

// New creates a DRAFT invoice, rejecting facts that break an invariant. All violations are
// reported together so a caller fixes one form, not one field per round trip.
func New(p NewParams, now time.Time) (*Invoice, error) {
	var violations []error

	if p.ID == uuid.Nil {
		violations = append(violations, apperr.Invalid("id", "must be a non-nil UUID"))
	}
	if p.IssuerID == uuid.Nil {
		violations = append(violations, apperr.Invalid("issuer_id", "must be a non-nil UUID"))
	}

	debtorRef := strings.TrimSpace(p.DebtorRef)
	switch {
	case debtorRef == "":
		violations = append(violations, apperr.Invalid("debtor_ref", "must not be empty"))
	case len(debtorRef) > MaxDebtorRefLen:
		violations = append(violations, apperr.Invalid("debtor_ref", "must be at most %d characters", MaxDebtorRefLen))
	}

	number := strings.TrimSpace(p.Number)
	switch {
	case number == "":
		violations = append(violations, apperr.Invalid("number", "must not be empty"))
	case len(number) > MaxNumberLen:
		violations = append(violations, apperr.Invalid("number", "must be at most %d characters", MaxNumberLen))
	}

	switch {
	case !p.Face.IsValid():
		violations = append(violations, apperr.Invalid("face", "must carry a supported currency"))
	case !p.Face.IsPositive():
		violations = append(violations, apperr.Invalid("face", "must be greater than zero, got %s", p.Face))
	}

	violations = append(violations, validateDates(p.IssuedAt, p.DueAt)...)

	if err := errors.Join(violations...); err != nil {
		return nil, err
	}

	issuedAt := p.IssuedAt.UTC()
	return &Invoice{
		ID:        p.ID,
		IssuerID:  p.IssuerID,
		DebtorRef: debtorRef,
		Number:    number,
		Face:      p.Face,
		IssuedAt:  issuedAt,
		DueAt:     p.DueAt.UTC(),
		Status:    StatusDraft,
		Version:   1,
		CreatedAt: now.UTC(),
		UpdatedAt: now.UTC(),
	}, nil
}

func validateDates(issuedAt, dueAt time.Time) []error {
	var violations []error

	if issuedAt.IsZero() {
		violations = append(violations, apperr.Invalid("issued_at", "must be set"))
	}
	if dueAt.IsZero() {
		violations = append(violations, apperr.Invalid("due_at", "must be set"))
	}
	if issuedAt.IsZero() || dueAt.IsZero() {
		return violations
	}

	if !dueAt.After(issuedAt) {
		violations = append(violations, apperr.Invalid("due_at", "must be after issued_at"))
		return violations
	}

	switch tenor := tenorDays(issuedAt, dueAt); {
	case tenor < MinTenorDays:
		violations = append(violations, apperr.Invalid("due_at", "tenor must be at least %d day(s)", MinTenorDays))
	case tenor > MaxTenorDays:
		violations = append(violations, apperr.Invalid("due_at", "tenor must be at most %d days, got %d", MaxTenorDays, tenor))
	}
	return violations
}

// Currency returns the invoice currency.
func (i *Invoice) Currency() money.Currency { return i.Face.Currency() }

// TenorDays is the financed period in whole days, from issue to due date. Pricing uses it
// directly, so it is defined once here rather than recomputed per caller.
func (i *Invoice) TenorDays() int64 { return tenorDays(i.IssuedAt, i.DueAt) }

// DaysToDue is the number of whole days between now and the due date. It is negative for
// an overdue invoice, which is what the maturity check needs.
func (i *Invoice) DaysToDue(now time.Time) int64 { return tenorDays(now, i.DueAt) }

// IsOverdue reports whether the due date has passed.
func (i *Invoice) IsOverdue(now time.Time) bool { return now.After(i.DueAt) }

// tenorDays truncates towards zero, so a receivable is only counted as another day old
// once that day has fully elapsed.
func tenorDays(from, to time.Time) int64 {
	return int64(to.Sub(from) / (24 * time.Hour))
}
