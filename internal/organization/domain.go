// Package organization owns the demo participants: an issuer that sells receivables, an
// investor that buys them, and the platform operator.
//
// Eligibility here is a demo gate, not KYC. The specification puts production KYC/AML
// outside the MVP, so this package models the state machine an eligibility decision moves
// through and nothing about how a real decision would be made.
package organization

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/GoldFridge/factorflow/internal/platform/apperr"
)

// Limits on organization facts.
const (
	// MaxNameLen bounds a display name.
	MaxNameLen = 200
	// MaxReasonLen bounds an eligibility decision note.
	MaxReasonLen = 512
)

// walletAddress matches the EVM-style addresses the Hedera-compatible wallets present.
var walletAddress = regexp.MustCompile(`^0x[0-9a-fA-F]{40}$`)

// Type is the role an organization plays in the market.
type Type string

// The organization types.
const (
	TypeIssuer   Type = "ISSUER"
	TypeInvestor Type = "INVESTOR"
	TypeOperator Type = "OPERATOR"
)

// IsValid reports whether the type is known.
func (t Type) IsValid() bool {
	switch t {
	case TypeIssuer, TypeInvestor, TypeOperator:
		return true
	default:
		return false
	}
}

// String returns the wire representation.
func (t Type) String() string { return string(t) }

// ParseType validates a type from storage or a request.
func ParseType(s string) (Type, error) {
	t := Type(s)
	if !t.IsValid() {
		return "", fmt.Errorf("%w: unknown organization type %q", apperr.ErrValidation, s)
	}
	return t, nil
}

// Eligibility is the demo participation gate.
type Eligibility string

// The eligibility states.
const (
	EligibilityPending  Eligibility = "PENDING"
	EligibilityEligible Eligibility = "ELIGIBLE"
	EligibilityRejected Eligibility = "REJECTED"
)

// IsValid reports whether the eligibility value is known.
func (e Eligibility) IsValid() bool {
	switch e {
	case EligibilityPending, EligibilityEligible, EligibilityRejected:
		return true
	default:
		return false
	}
}

// String returns the wire representation.
func (e Eligibility) String() string { return string(e) }

// ParseEligibility validates an eligibility value from storage or a request.
func ParseEligibility(s string) (Eligibility, error) {
	e := Eligibility(s)
	if !e.IsValid() {
		return "", fmt.Errorf("%w: unknown eligibility %q", apperr.ErrValidation, s)
	}
	return e, nil
}

// Organization is a demo participant.
type Organization struct {
	ID          uuid.UUID
	Type        Type
	Name        string
	Wallet      string
	Eligibility Eligibility
	Reason      string

	// ChainAccountID is where this participant's tokens are delivered. It is empty until
	// the platform has opened an account for them: a wallet address proves who signed, and
	// is not somewhere a token can be sent.
	ChainAccountID string

	Version   int64
	CreatedAt time.Time
	UpdatedAt time.Time
}

// NewParams carries the facts supplied when an organization is created.
type NewParams struct {
	ID     uuid.UUID
	Type   Type
	Name   string
	Wallet string
}

// New creates an organization awaiting an eligibility decision.
func New(p NewParams, now time.Time) (*Organization, error) {
	var violations []error

	if p.ID == uuid.Nil {
		violations = append(violations, apperr.Invalid("id", "must be a non-nil UUID"))
	}
	if !p.Type.IsValid() {
		violations = append(violations, apperr.Invalid("type", "must be one of ISSUER, INVESTOR, OPERATOR"))
	}

	name := strings.TrimSpace(p.Name)
	switch {
	case name == "":
		violations = append(violations, apperr.Invalid("name", "must not be empty"))
	case len(name) > MaxNameLen:
		violations = append(violations, apperr.Invalid("name", "must be at most %d characters", MaxNameLen))
	}

	wallet := strings.TrimSpace(p.Wallet)
	if !walletAddress.MatchString(wallet) {
		violations = append(violations, apperr.Invalid("wallet", "must be a 0x-prefixed 20-byte address"))
	}

	if err := errors.Join(violations...); err != nil {
		return nil, err
	}

	return &Organization{
		ID:          p.ID,
		Type:        p.Type,
		Name:        name,
		Wallet:      strings.ToLower(wallet),
		Eligibility: EligibilityPending,
		Version:     1,
		CreatedAt:   now.UTC(),
		UpdatedAt:   now.UTC(),
	}, nil
}

// Approve records a passed demo eligibility check.
func (o *Organization) Approve(now time.Time) error {
	if o.Eligibility == EligibilityEligible {
		return apperr.Conflictf("organization %s is already eligible", o.ID)
	}
	o.Eligibility = EligibilityEligible
	o.Reason = ""
	o.touch(now)
	return nil
}

// Reject records a failed demo eligibility check.
func (o *Organization) Reject(reason string, now time.Time) error {
	cleaned := strings.TrimSpace(reason)
	switch {
	case cleaned == "":
		return apperr.Invalid("reason", "must not be empty")
	case len(cleaned) > MaxReasonLen:
		return apperr.Invalid("reason", "must be at most %d characters", MaxReasonLen)
	}
	if o.Eligibility == EligibilityRejected {
		return apperr.Conflictf("organization %s is already rejected", o.ID)
	}

	o.Eligibility = EligibilityRejected
	o.Reason = cleaned
	o.touch(now)
	return nil
}

// Rename changes the display name.
func (o *Organization) Rename(name string, now time.Time) error {
	cleaned := strings.TrimSpace(name)
	switch {
	case cleaned == "":
		return apperr.Invalid("name", "must not be empty")
	case len(cleaned) > MaxNameLen:
		return apperr.Invalid("name", "must be at most %d characters", MaxNameLen)
	}

	o.Name = cleaned
	o.touch(now)
	return nil
}

// IsEligible reports whether the organization may trade.
func (o *Organization) IsEligible() bool { return o.Eligibility == EligibilityEligible }

// CanIssue reports whether the organization may create invoices.
func (o *Organization) CanIssue() bool { return o.Type == TypeIssuer && o.IsEligible() }

// CanInvest reports whether the organization may place bids.
func (o *Organization) CanInvest() bool { return o.Type == TypeInvestor && o.IsEligible() }

func (o *Organization) touch(now time.Time) {
	o.Version++
	o.UpdatedAt = now.UTC()
}

/*
AttachChainAccount records the account this participant receives tokens at.

It is set once. An account that could be repointed is a way to redirect somebody else's
holdings, and nothing in this demo needs it to change: an account that has to move is a
participant with a new account, not an old one with a new address.
*/
func (o *Organization) AttachChainAccount(accountID string, now time.Time) error {
	account := strings.TrimSpace(accountID)
	if account == "" {
		return apperr.Invalid("chain_account_id", "must not be empty")
	}
	if o.ChainAccountID != "" && o.ChainAccountID != account {
		return apperr.Conflictf("organization %s already receives tokens at %s",
			o.ID, o.ChainAccountID)
	}

	o.ChainAccountID = account
	o.Version++
	o.UpdatedAt = now.UTC()
	return nil
}

// Receives is where tokens for this participant are delivered: the account the platform
// opened, or the wallet address when there is none, which is what the in-process ledger uses.
func (o *Organization) Receives() string {
	if o.ChainAccountID != "" {
		return o.ChainAccountID
	}
	return o.Wallet
}
