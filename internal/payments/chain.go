package payments

import (
	"context"
	"strings"
	"time"

	"github.com/GoldFridge/factorflow/internal/platform/apperr"
)

/*
Ledger is what this module needs from a network to believe a payment happened.

One question, asked of something that did not take part in the payment. That is the whole
design: the platform is not trusting the payer's word, its own record of what it quoted, or
a third party's assurance — it is reading the network.
*/
type Ledger interface {
	// Payment returns what the network holds for a transaction. A transaction the ledger
	// has not seen is reported as absent rather than as an error: a mirror lags consensus,
	// and "not yet" is a different answer from "never".
	Payment(ctx context.Context, transactionID string) (LedgerPayment, error)
	// Network names the chain, so a quote can tell a client where to pay.
	Network() string
}

// LedgerPayment is one transaction as the network describes it.
type LedgerPayment struct {
	TransactionID string
	Found         bool
	Succeeded     bool
	Status        string
	ConfirmedAt   time.Time
	// Memo is the note the payer attached, which is where the binding to one quote lives.
	Memo string
	// Paid reports whether an account was credited with at least an amount, in the
	// network's smallest unit.
	Paid func(account string, minor int64) bool
	// PaidBy reports whether an account was debited, which is how the payer is checked
	// against the one the proof claims.
	PaidBy func(account string) bool
}

/*
ChainFacilitator verifies x402 payments against a network.

The five checks are the ones the specification names, and each exists because of a specific
way a machine customer could be served an answer it did not pay for:

  - the transaction happened and the network says it succeeded, so a fabricated id buys
    nothing;
  - it credited this platform's account, so paying somebody else does not count;
  - it credited at least the quoted price, so a smaller payment does not buy a whole answer;
  - it was submitted by the payer the proof names, so one agent's payment does not buy
    another's answer;
  - its memo carries the nonce this quote was issued under, so a payment for one question
    cannot buy the answer to a different one — and, with the nonce spent on first use,
    cannot buy the same answer twice.

Freshness is the sixth, and it is the quote's own: a price is only a price for as long as it
was quoted for.
*/
type ChainFacilitator struct {
	ledger    Ledger
	recipient string
	now       func() time.Time
}

// ClockSkewTolerance is how far the network's clock may run behind this platform's before a
// payment is treated as too old to belong to a quote.
const ClockSkewTolerance = 5 * time.Minute

// NewChainFacilitator returns the facilitator.
func NewChainFacilitator(ledger Ledger, recipient string, now func() time.Time) *ChainFacilitator {
	if now == nil {
		now = time.Now
	}
	return &ChainFacilitator{
		ledger:    ledger,
		recipient: strings.TrimSpace(recipient),
		now:       now,
	}
}

// Recipient is the account payments must reach.
func (f *ChainFacilitator) Recipient() string { return f.recipient }

// Verify reads the network and refuses anything it cannot confirm.
func (f *ChainFacilitator) Verify(ctx context.Context, requirement Requirement, payment Payment) error {
	if requirement.IsExpired(f.now()) {
		return apperr.Forbiddenf("the quoted price expired at %s",
			requirement.ExpiresAt.Format(time.RFC3339))
	}
	if strings.TrimSpace(payment.TxID) == "" {
		return apperr.Invalid("tx_id", "a payment must name the transaction that made it")
	}

	record, err := f.ledger.Payment(ctx, payment.TxID)
	if err != nil {
		// A mirror that cannot be reached is not a refusal: the client is told to try again
		// rather than told its payment was bad.
		return apperr.Unavailablef("the network could not be asked about %s: %v", payment.TxID, err)
	}
	switch {
	case !record.Found:
		return apperr.Unavailablef("transaction %s has not appeared on %s yet",
			payment.TxID, f.ledger.Network())
	case !record.Succeeded:
		return apperr.Forbiddenf("the network rejected transaction %s (%s)",
			payment.TxID, record.Status)
	}

	if record.Paid == nil || !record.Paid(f.recipient, requirement.Price.Minor()) {
		return apperr.Forbiddenf("transaction %s did not pay %s to %s",
			payment.TxID, requirement.Price, f.recipient)
	}

	payer := strings.TrimSpace(payment.Payer)
	if payer != "" && record.PaidBy != nil && !record.PaidBy(payer) {
		return apperr.Forbiddenf("transaction %s was not paid by %s", payment.TxID, payer)
	}

	if strings.TrimSpace(record.Memo) != strings.TrimSpace(requirement.Nonce) {
		return apperr.Forbiddenf(
			"transaction %s is not bound to this quote: its memo must carry the nonce %s",
			payment.TxID, requirement.Nonce)
	}

	// A payment made long before the quote existed cannot have been made for it. This is a
	// secondary guard: the memo already carries a nonce the payer could not have known in
	// advance, so an old transfer cannot be dressed up as a new one whatever its timestamp.
	//
	// It is generous on purpose, and that is not laziness. The two timestamps come from two
	// clocks — this platform's and the network's consensus — and they are never exactly
	// aligned. A strict comparison refuses honest payments whenever the server's clock runs
	// a minute fast, which is a failure a machine customer cannot diagnose, cannot fix, and
	// has already paid for.
	if !record.ConfirmedAt.IsZero() &&
		record.ConfirmedAt.Before(requirement.IssuedAt.Add(-ClockSkewTolerance)) {
		return apperr.Forbiddenf("transaction %s was confirmed long before this quote was issued",
			payment.TxID)
	}
	return nil
}

/*
Settle records that the exchange is closed.

There is nothing to capture: on this network a transfer is final when the network agrees it
happened, and Verify has already read that. The step stays because a chain that separates
authorization from capture would need it, and a facilitator interface that pretended
otherwise would push that difference into every caller.
*/
func (f *ChainFacilitator) Settle(ctx context.Context, payment Payment) error {
	record, err := f.ledger.Payment(ctx, payment.TxID)
	if err != nil {
		return apperr.Unavailablef("the network could not be asked about %s: %v", payment.TxID, err)
	}
	if !record.Succeeded {
		return apperr.Forbiddenf("transaction %s is not final", payment.TxID)
	}
	return nil
}
