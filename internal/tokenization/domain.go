// Package tokenization owns the tokenized asset: the on-chain representation of an
// approved receivable, and the lifecycle it moves through.
//
// The specification issues assets through Hedera Asset Tokenization Studio. That is one
// implementation of the Issuer port defined here; the in-process one mints a local
// identifier so the whole path works without a network. Which one is in use is decided at
// wiring time, and nothing in the domain can tell the difference.
package tokenization

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/GoldFridge/factorflow/internal/platform/apperr"
	"github.com/GoldFridge/factorflow/internal/platform/money"
)

// Limits on asset facts.
const (
	// MaxIdentifierLen bounds a chain identifier, which is whatever the network calls it.
	MaxIdentifierLen = 128
	// MaxURLLen bounds an explorer link.
	MaxURLLen = 512
)

// ChainStatus is where the asset stands on chain.
type ChainStatus string

// The chain states an asset moves through.
const (
	// StatusPending means issuance was submitted but not yet confirmed.
	StatusPending ChainStatus = "PENDING"
	// StatusIssued means the supply exists and can be transferred.
	StatusIssued ChainStatus = "ISSUED"
	// StatusFrozen means transfers are suspended by a compliance action.
	StatusFrozen ChainStatus = "FROZEN"
	// StatusRedeemed means the receivable was repaid and the supply retired.
	StatusRedeemed ChainStatus = "REDEEMED"
)

// transitions is the chain lifecycle. Redemption is terminal: a repaid receivable has
// nothing left to represent.
var transitions = map[ChainStatus][]ChainStatus{
	StatusPending:  {StatusIssued},
	StatusIssued:   {StatusFrozen, StatusRedeemed},
	StatusFrozen:   {StatusIssued, StatusRedeemed},
	StatusRedeemed: nil,
}

// ParseChainStatus validates a status read from storage.
func ParseChainStatus(s string) (ChainStatus, error) {
	status := ChainStatus(s)
	if !status.IsValid() {
		return "", fmt.Errorf("%w: unknown chain status %q", apperr.ErrValidation, s)
	}
	return status, nil
}

// IsValid reports whether the status is part of the lifecycle.
func (s ChainStatus) IsValid() bool {
	_, ok := transitions[s]
	return ok
}

// CanTransitionTo reports whether next is reachable from s in one step.
func (s ChainStatus) CanTransitionTo(next ChainStatus) bool {
	for _, allowed := range transitions[s] {
		if allowed == next {
			return true
		}
	}
	return false
}

// String returns the wire representation.
func (s ChainStatus) String() string { return string(s) }

// Asset is the tokenized representation of one receivable.
type Asset struct {
	ID        uuid.UUID
	InvoiceID uuid.UUID
	IssuerID  uuid.UUID

	// Network names the chain the asset lives on, or "local" for the in-process issuer.
	Network string
	// TokenID is the asset's identifier on that network.
	TokenID string
	// ContractID is the compliance contract behind it, where the network has one.
	ContractID string

	// Supply is the notional the asset represents, in the invoice's currency.
	Supply money.Amount

	ChainStatus ChainStatus
	// TransactionID and ExplorerURL are the evidence a judge follows. They are stored
	// rather than derived: an explorer path is a property of the network, not of us.
	TransactionID string
	ExplorerURL   string

	CreatedAt time.Time
	UpdatedAt time.Time
}

// NewParams carries what issuance produced.
type NewParams struct {
	ID        uuid.UUID
	InvoiceID uuid.UUID
	IssuerID  uuid.UUID

	Network       string
	TokenID       string
	ContractID    string
	Supply        money.Amount
	ChainStatus   ChainStatus
	TransactionID string
	ExplorerURL   string
}

// New validates and builds an asset record.
func New(p NewParams, now time.Time) (*Asset, error) {
	var violations []error

	if p.ID == uuid.Nil {
		violations = append(violations, apperr.Invalid("id", "must be a non-nil UUID"))
	}
	if p.InvoiceID == uuid.Nil {
		violations = append(violations, apperr.Invalid("invoice_id", "must be a non-nil UUID"))
	}
	if p.IssuerID == uuid.Nil {
		violations = append(violations, apperr.Invalid("issuer_id", "must be a non-nil UUID"))
	}

	violations = append(violations, validateIdentifier("network", p.Network)...)
	violations = append(violations, validateIdentifier("token_id", p.TokenID)...)

	if len(p.ContractID) > MaxIdentifierLen {
		violations = append(violations, apperr.Invalid("contract_id", "must be at most %d characters", MaxIdentifierLen))
	}
	if len(p.TransactionID) > MaxIdentifierLen {
		violations = append(violations, apperr.Invalid("transaction_id", "must be at most %d characters", MaxIdentifierLen))
	}
	if len(p.ExplorerURL) > MaxURLLen {
		violations = append(violations, apperr.Invalid("explorer_url", "must be at most %d characters", MaxURLLen))
	}

	switch {
	case !p.Supply.IsValid():
		violations = append(violations, apperr.Invalid("supply", "must carry a supported currency"))
	case !p.Supply.IsPositive():
		violations = append(violations, apperr.Invalid("supply", "must be greater than zero"))
	}

	status := p.ChainStatus
	if status == "" {
		status = StatusPending
	}
	if !status.IsValid() {
		violations = append(violations, apperr.Invalid("chain_status", "unknown chain status %q", p.ChainStatus))
	}

	if err := errors.Join(violations...); err != nil {
		return nil, err
	}

	return &Asset{
		ID:            p.ID,
		InvoiceID:     p.InvoiceID,
		IssuerID:      p.IssuerID,
		Network:       strings.TrimSpace(p.Network),
		TokenID:       strings.TrimSpace(p.TokenID),
		ContractID:    strings.TrimSpace(p.ContractID),
		Supply:        p.Supply,
		ChainStatus:   status,
		TransactionID: strings.TrimSpace(p.TransactionID),
		ExplorerURL:   strings.TrimSpace(p.ExplorerURL),
		CreatedAt:     now.UTC(),
		UpdatedAt:     now.UTC(),
	}, nil
}

// MarkIssued records a confirmed issuance.
func (a *Asset) MarkIssued(transactionID, explorerURL string, now time.Time) error {
	if err := a.transition(StatusIssued, now); err != nil {
		return err
	}
	if transactionID != "" {
		a.TransactionID = strings.TrimSpace(transactionID)
	}
	if explorerURL != "" {
		a.ExplorerURL = strings.TrimSpace(explorerURL)
	}
	return nil
}

// Freeze suspends transfers, which is the compliance control the specification calls for.
func (a *Asset) Freeze(now time.Time) error { return a.transition(StatusFrozen, now) }

// Unfreeze restores transfers.
func (a *Asset) Unfreeze(now time.Time) error { return a.transition(StatusIssued, now) }

// Redeem retires the supply once the receivable is repaid.
func (a *Asset) Redeem(now time.Time) error { return a.transition(StatusRedeemed, now) }

// IsTransferable reports whether settlement may move this asset.
func (a *Asset) IsTransferable() bool { return a.ChainStatus == StatusIssued }

func (a *Asset) transition(next ChainStatus, now time.Time) error {
	if !a.ChainStatus.CanTransitionTo(next) {
		return apperr.Conflictf("asset %s cannot move from %s to %s", a.ID, a.ChainStatus, next)
	}
	a.ChainStatus = next
	a.UpdatedAt = now.UTC()
	return nil
}

func validateIdentifier(field, value string) []error {
	trimmed := strings.TrimSpace(value)
	switch {
	case trimmed == "":
		return []error{apperr.Invalid(field, "must not be empty")}
	case len(trimmed) > MaxIdentifierLen:
		return []error{apperr.Invalid(field, "must be at most %d characters", MaxIdentifierLen)}
	default:
		return nil
	}
}
