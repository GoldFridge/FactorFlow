// Package settlement moves an allocated receivable from its issuer to the investor that
// bought it.
//
// PostgreSQL and a chain cannot share a transaction. A transfer that succeeded on chain
// cannot be rolled back because the local write after it failed, and a local write cannot
// be trusted to mean the transfer happened. So settlement is a saga: each step is durable,
// each is safe to repeat, and the row itself is the memory between them.
//
// Compensation here never pretends an irreversible operation did not happen. When the
// local state and the chain disagree, the chain is the truth and the local state is
// recomputed from it.
package settlement

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/GoldFridge/factorflow/internal/platform/apperr"
	"github.com/GoldFridge/factorflow/internal/platform/money"
)

// MaxErrorLen bounds the stored cause of a failure.
const MaxErrorLen = 512

// State is one step of the saga.
type State string

// The saga's steps, in order.
const (
	// StatePrepared is a planned transfer that has not been submitted. Nothing has
	// happened on chain yet, so this is the only state a plan can be abandoned from.
	StatePrepared State = "PREPARED"
	// StateSubmitted means a transaction was sent. This is the dangerous state: the
	// transfer may or may not have taken effect, so the next step asks rather than retries.
	StateSubmitted State = "SUBMITTED"
	// StateConsensusConfirmed means the network accepted the transaction.
	StateConsensusConfirmed State = "CONSENSUS_CONFIRMED"
	// StateMirrorConfirmed means an independent read of the chain confirms it.
	StateMirrorConfirmed State = "MIRROR_CONFIRMED"
	// StateAccounted means the local records were updated to match.
	StateAccounted State = "ACCOUNTED"
	// StateFailed is a plan that could not be carried out. It does not mean nothing
	// happened on chain: it means the local state must be reconciled against the chain.
	StateFailed State = "FAILED"
)

// transitions is the whole saga. A step not listed here cannot happen, whatever the
// calling code believes.
//
// Note what is missing: nothing leads back to PREPARED. Once a transaction has been
// submitted the plan can never again be treated as unstarted, because re-submitting is how
// one allocation becomes two transfers.
var transitions = map[State][]State{
	StatePrepared:           {StateSubmitted, StateFailed},
	StateSubmitted:          {StateConsensusConfirmed, StateFailed},
	StateConsensusConfirmed: {StateMirrorConfirmed, StateFailed},
	StateMirrorConfirmed:    {StateAccounted, StateFailed},
	StateAccounted:          nil,
	StateFailed:             {StateSubmitted, StateConsensusConfirmed, StateMirrorConfirmed, StateAccounted},
}

// ParseState validates a state read from storage or a request.
func ParseState(s string) (State, error) {
	state := State(s)
	if !state.IsValid() {
		return "", apperr.Invalid("state", "unknown settlement state %q", s)
	}
	return state, nil
}

// IsValid reports whether the state is part of the saga.
func (s State) IsValid() bool {
	_, ok := transitions[s]
	return ok
}

// IsFinished reports whether the settlement needs no further work.
func (s State) IsFinished() bool { return s == StateAccounted }

// CanTransitionTo reports whether next is reachable from s in one step.
func (s State) CanTransitionTo(next State) bool {
	for _, allowed := range transitions[s] {
		if allowed == next {
			return true
		}
	}
	return false
}

// String returns the wire form.
func (s State) String() string { return string(s) }

// Settlement is one allocated lot on its way from the issuer to its investor.
type Settlement struct {
	ID uuid.UUID

	// AuctionID, LotID and BidID name the allocation this settles. Together they are the
	// allocation's identity, so one allocation can only ever have one settlement.
	AuctionID uuid.UUID
	LotID     uuid.UUID
	BidID     uuid.UUID

	InvoiceID  uuid.UUID
	AssetID    uuid.UUID
	InvestorID uuid.UUID

	FromWallet string
	ToWallet   string

	// Notional is the face value transferred; Price is what the investor pays for it.
	Notional money.Amount
	Price    money.Amount

	// OperationID is derived from the plan rather than generated, so a resubmission of the
	// same transfer carries the same id and an executor can recognise work it already did.
	OperationID string
	// TxID is the chain's identifier for the submitted transaction, empty until submission.
	TxID string

	State State
	// Attempts counts submissions, not retries of the whole saga: it is the number of times
	// this transfer was actually pushed at the chain.
	Attempts int
	// LastError is the most recent cause of failure, kept for an operator to read. It is
	// never shown to a counterparty.
	LastError string

	Version   int64
	CreatedAt time.Time
	UpdatedAt time.Time
}

// NewParams are the facts of a planned transfer.
type NewParams struct {
	ID        uuid.UUID
	AuctionID uuid.UUID
	LotID     uuid.UUID
	BidID     uuid.UUID

	InvoiceID  uuid.UUID
	AssetID    uuid.UUID
	InvestorID uuid.UUID

	FromWallet string
	ToWallet   string

	Notional money.Amount
	Price    money.Amount
}

// New plans a transfer.
func New(p NewParams, now time.Time) (*Settlement, error) {
	var violations []error

	for _, id := range []struct {
		name  string
		value uuid.UUID
	}{
		{"id", p.ID}, {"auction_id", p.AuctionID}, {"lot_id", p.LotID}, {"bid_id", p.BidID},
		{"invoice_id", p.InvoiceID}, {"asset_id", p.AssetID}, {"investor_id", p.InvestorID},
	} {
		if id.value == uuid.Nil {
			violations = append(violations, apperr.Invalid(id.name, "must be a non-nil UUID"))
		}
	}

	from, to := strings.ToLower(strings.TrimSpace(p.FromWallet)), strings.ToLower(strings.TrimSpace(p.ToWallet))
	switch {
	case from == "":
		violations = append(violations, apperr.Invalid("from_wallet", "must not be empty"))
	case to == "":
		violations = append(violations, apperr.Invalid("to_wallet", "must not be empty"))
	case from == to:
		// A transfer to itself would settle on chain and move nothing, leaving an investor
		// with a paid-for allocation they never received.
		violations = append(violations, apperr.Invalid("to_wallet", "must differ from the sending wallet"))
	}

	if !p.Notional.IsValid() || !p.Notional.IsPositive() {
		violations = append(violations, apperr.Invalid("notional", "must be a positive amount"))
	}
	if !p.Price.IsValid() || !p.Price.IsPositive() {
		violations = append(violations, apperr.Invalid("price", "must be a positive amount"))
	}
	if p.Notional.IsValid() && p.Price.IsValid() && p.Notional.Currency() != p.Price.Currency() {
		violations = append(violations, apperr.Invalid("price",
			"must be in %s, the currency of the notional", p.Notional.Currency()))
	}

	if err := errors.Join(violations...); err != nil {
		return nil, err
	}

	s := &Settlement{
		ID:         p.ID,
		AuctionID:  p.AuctionID,
		LotID:      p.LotID,
		BidID:      p.BidID,
		InvoiceID:  p.InvoiceID,
		AssetID:    p.AssetID,
		InvestorID: p.InvestorID,
		FromWallet: from,
		ToWallet:   to,
		Notional:   p.Notional,
		Price:      p.Price,
		State:      StatePrepared,
		Version:    1,
		CreatedAt:  now.UTC(),
		UpdatedAt:  now.UTC(),
	}
	s.OperationID = OperationID(s)
	return s, nil
}

// OperationID derives the deterministic identifier of a transfer.
//
// It is computed from what the transfer does — which allocation, which asset, between
// which wallets, for how much — and not from when it was attempted. Two runs of the same
// plan therefore produce the same id, which is what lets an executor recognise a transfer
// it has already performed instead of performing it twice.
func OperationID(s *Settlement) string {
	parts := []string{
		"factorflow.settlement.v1",
		s.AuctionID.String(),
		s.LotID.String(),
		s.BidID.String(),
		s.AssetID.String(),
		s.FromWallet,
		s.ToWallet,
		s.Notional.String(),
		s.Notional.Currency().String(),
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "|")))
	return "0x" + hex.EncodeToString(sum[:])
}

// Submit records that a transaction was sent to the chain.
//
// The transaction id is required even though the outcome is not yet known. A submission
// whose id was not recorded is the one failure the saga cannot recover from: nothing later
// could ask the chain what happened to it.
func (s *Settlement) Submit(txID string, now time.Time) error {
	if strings.TrimSpace(txID) == "" {
		return apperr.Invalid("tx_id", "must not be empty")
	}
	if err := s.transition(StateSubmitted, now); err != nil {
		return err
	}

	s.TxID = strings.TrimSpace(txID)
	s.Attempts++
	s.LastError = ""
	return nil
}

// ConfirmConsensus records that the network accepted the transaction.
func (s *Settlement) ConfirmConsensus(now time.Time) error {
	return s.transition(StateConsensusConfirmed, now)
}

// ConfirmMirror records that an independent read of the chain sees the transfer.
func (s *Settlement) ConfirmMirror(now time.Time) error {
	return s.transition(StateMirrorConfirmed, now)
}

// Account records that the local books were brought in line with the chain.
func (s *Settlement) Account(now time.Time) error {
	return s.transition(StateAccounted, now)
}

// Fail records why the saga stopped.
//
// It does not undo anything: a transfer that reached the chain stays there, and the reason
// is kept so reconciliation can decide what the local state should become.
func (s *Settlement) Fail(cause string, now time.Time) error {
	cleaned := strings.TrimSpace(cause)
	switch {
	case cleaned == "":
		return apperr.Invalid("cause", "must not be empty")
	case len(cleaned) > MaxErrorLen:
		cleaned = cleaned[:MaxErrorLen]
	}
	if s.State == StateAccounted {
		return apperr.Conflictf("settlement %s is already accounted for", s.ID)
	}

	s.State = StateFailed
	s.LastError = cleaned
	s.touch(now)
	return nil
}

// IsSubmitted reports whether a transaction was ever pushed to the chain for this
// settlement. A caller that has to decide between retrying and asking must ask when this
// is true.
func (s *Settlement) IsSubmitted() bool { return s.TxID != "" }

// IsFinished reports whether the saga is complete.
func (s *Settlement) IsFinished() bool { return s.State.IsFinished() }

func (s *Settlement) transition(next State, now time.Time) error {
	if !s.State.CanTransitionTo(next) {
		return apperr.Conflictf("settlement %s cannot move from %s to %s", s.ID, s.State, next)
	}
	// Resuming after a failure may not skip a step it never reached: a settlement that
	// failed before submission has no transaction to confirm.
	if s.State == StateFailed && next != StateSubmitted && !s.IsSubmitted() {
		return apperr.Conflictf("settlement %s was never submitted, so it cannot be confirmed", s.ID)
	}

	s.State = next
	s.touch(now)
	return nil
}

func (s *Settlement) touch(now time.Time) {
	s.Version++
	s.UpdatedAt = now.UTC()
}

// String is the operator-facing summary of a settlement.
func (s *Settlement) String() string {
	return fmt.Sprintf("settlement %s (%s) %s %s → %s", s.ID, s.State, s.Notional, s.FromWallet, s.ToWallet)
}
