package payments

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"
	"time"

	"github.com/GoldFridge/factorflow/internal/platform/apperr"
	"github.com/GoldFridge/factorflow/internal/platform/money"
)

// SchemeExact names the scheme where the client pays the exact quoted amount.
const SchemeExact = "exact"

// LocalNetwork names the in-process ledger, so a payment made without a chain is never
// mistaken for one made on a real network.
const LocalNetwork = "local"

// Facilitator decides whether a payment is good for a quote.
//
// It answers five questions, and all five matter: is the payment real, did it go to the
// right recipient, is it enough, is it for this request, and has it been spent before.
// Dropping any one of them turns the endpoint into a free one for a client that noticed.
type Facilitator interface {
	Verify(ctx context.Context, requirement Requirement, payment Payment) error
	// Settle makes the payment final. On a chain that separates authorization from
	// capture this is where the capture happens; where it does not, it records that the
	// platform considers the exchange closed.
	Settle(ctx context.Context, payment Payment) error
}

// LocalFacilitator is an in-process facilitator for development and tests.
//
// It keeps a ledger of payments made through LocalPayer and enforces the same five checks
// a real one must. In particular it refuses a payment it has already accepted, because
// spending one payment on two answers is the failure this whole exchange exists to prevent.
type LocalFacilitator struct {
	mu sync.Mutex
	// ledger holds the payments a payer has made, by transaction id.
	ledger map[string]Payment
	// spent records which payments have already bought an answer.
	spent map[string]string

	recipient string
	now       func() time.Time

	failVerify error
}

// NewLocalFacilitator returns the in-process facilitator paying to recipient.
func NewLocalFacilitator(recipient string, now func() time.Time) *LocalFacilitator {
	if now == nil {
		now = time.Now
	}
	return &LocalFacilitator{
		ledger:    map[string]Payment{},
		spent:     map[string]string{},
		recipient: strings.ToLower(strings.TrimSpace(recipient)),
		now:       now,
	}
}

// Pay makes a payment for a quote, as a client's wallet would.
//
// It exists so a demo and a test can complete the exchange without a chain. The
// transaction id is derived from the payer, the nonce and the amount, so the same payment
// made twice is the same transaction rather than two.
func (f *LocalFacilitator) Pay(payer string, requirement Requirement) Payment {
	f.mu.Lock()
	defer f.mu.Unlock()

	payment := Payment{
		Nonce:  requirement.Nonce,
		Payer:  strings.ToLower(strings.TrimSpace(payer)),
		Amount: requirement.Price,
	}
	payment.TxID = localTxID(payment.Payer, requirement.Nonce, requirement.Price)

	f.ledger[payment.TxID] = payment
	return payment
}

// Verify performs the five checks.
func (f *LocalFacilitator) Verify(_ context.Context, requirement Requirement, payment Payment) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.failVerify != nil {
		return f.failVerify
	}

	recorded, ok := f.ledger[payment.TxID]
	if !ok {
		return apperr.Forbiddenf("payment %s is not on the ledger", payment.TxID)
	}
	if requirement.IsExpired(f.now()) {
		// A stale quote is refused even though the payment is real: the price it was made
		// against no longer stands.
		return apperr.Forbiddenf("the quoted price expired at %s", requirement.ExpiresAt.Format(time.RFC3339))
	}
	if recorded.Nonce != requirement.Nonce {
		return apperr.Forbiddenf("payment %s was made for another request", payment.TxID)
	}
	if recorded.Payer != strings.ToLower(strings.TrimSpace(payment.Payer)) {
		return apperr.Forbiddenf("payment %s was made by another payer", payment.TxID)
	}
	// A comparison across currencies fails rather than defaulting either way: a payment in
	// the wrong asset is not a small payment, it is not a payment for this quote at all.
	shortfall, err := recorded.Amount.Cmp(requirement.Price)
	if err != nil {
		return apperr.Forbiddenf("payment %s is in %s, not the quoted %s",
			payment.TxID, recorded.Amount.Currency(), requirement.Price.Currency())
	}
	if shortfall < 0 {
		return apperr.Forbiddenf("payment %s is %s, less than the quoted %s",
			payment.TxID, recorded.Amount, requirement.Price)
	}
	if spentOn, used := f.spent[payment.TxID]; used && spentOn != requirement.RequestHash {
		return apperr.Forbiddenf("payment %s has already been used", payment.TxID)
	}

	f.spent[payment.TxID] = requirement.RequestHash
	return nil
}

// Settle records the payment as final.
func (f *LocalFacilitator) Settle(_ context.Context, payment Payment) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if _, ok := f.ledger[payment.TxID]; !ok {
		return apperr.Forbiddenf("payment %s is not on the ledger", payment.TxID)
	}
	return nil
}

// Recipient is the address payments must go to.
func (f *LocalFacilitator) Recipient() string { return f.recipient }

// FailVerification makes every later verification fail with err. Passing nil restores
// normal behaviour.
func (f *LocalFacilitator) FailVerification(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.failVerify = err
}

// Forge records a payment that was never made, so a test can present a proof the ledger
// does not back.
func (f *LocalFacilitator) Forge(payment Payment) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.ledger[payment.TxID] = payment
}

// localTxID derives a transaction id from the payment, so a demo produces the same
// identifiers on every run.
func localTxID(payer, nonce string, amount money.Amount) string {
	sum := sha256.Sum256([]byte("local-payment|" + payer + "|" + nonce + "|" + amount.String()))
	return "pay-" + hex.EncodeToString(sum[:12])
}
