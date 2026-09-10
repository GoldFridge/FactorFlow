package payments_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoldFridge/factorflow/internal/payments"
	"github.com/GoldFridge/factorflow/internal/platform/apperr"
	"github.com/GoldFridge/factorflow/internal/platform/money"
)

const (
	platformAccount = "0.0.10419676"
	agentAccount    = "0.0.10456604"
)

// fakeLedger answers whatever a test says the network holds.
type fakeLedger struct {
	payment payments.LedgerPayment
	err     error
}

func (f fakeLedger) Network() string { return "testnet" }

func (f fakeLedger) Payment(context.Context, string) (payments.LedgerPayment, error) {
	return f.payment, f.err
}

// quoted is the price a machine customer was asked for.
func chainQuote(t *testing.T) payments.Requirement {
	t.Helper()

	return payments.Requirement{
		Scheme:      "hedera-transfer",
		Network:     "testnet",
		Recipient:   platformAccount,
		Asset:       "HBAR",
		Price:       money.MustParse("0.25", money.HBAR),
		Nonce:       "7f3a9c21",
		RequestHash: "0xabc",
		IssuedAt:    chainNow.Add(-time.Minute),
		ExpiresAt:   chainNow.Add(4 * time.Minute),
	}
}

var chainNow = time.Date(2026, time.September, 10, 12, 0, 0, 0, time.UTC)

// settled is a network record of the payment that quote asked for.
func settled() payments.LedgerPayment {
	return payments.LedgerPayment{
		TransactionID: "0.0.10456604@1789039541.207925088",
		Found:         true,
		Succeeded:     true,
		Status:        "SUCCESS",
		ConfirmedAt:   chainNow.Add(-30 * time.Second),
		Memo:          "7f3a9c21",
		Paid: func(account string, minor int64) bool {
			return account == platformAccount && minor <= 25_000_000
		},
		PaidBy: func(account string) bool { return account == agentAccount },
	}
}

func verify(t *testing.T, ledger payments.Ledger, mutate func(*payments.Payment)) error {
	t.Helper()

	payment := payments.Payment{
		Nonce: "7f3a9c21",
		Payer: agentAccount,
		TxID:  "0.0.10456604@1789039541.207925088",
	}
	if mutate != nil {
		mutate(&payment)
	}

	return payments.NewChainFacilitator(ledger, platformAccount, func() time.Time { return chainNow }).
		Verify(context.Background(), chainQuote(t), payment)
}

// TestAPaymentTheNetworkConfirmsIsAccepted is the ordinary path: the machine paid, and the
// platform read that from something that did not take part in the payment.
func TestAPaymentTheNetworkConfirmsIsAccepted(t *testing.T) {
	t.Parallel()

	require.NoError(t, verify(t, fakeLedger{payment: settled()}, nil))
}

/*
 * TestEachCheckRefusesItsOwnWayOfNotPaying. Every case here is a way a machine customer
 * could otherwise be handed an answer it did not buy, so each is refused for its own reason
 * rather than by one blanket rejection nobody can debug.
 */
func TestEachCheckRefusesItsOwnWayOfNotPaying(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		ledger  payments.Ledger
		mutate  func(*payments.Payment)
		wantErr error
		says    string
	}{
		{
			name: "a transaction the network never saw",
			ledger: fakeLedger{payment: payments.LedgerPayment{
				TransactionID: "0.0.10456604@1789039541.207925088",
			}},
			wantErr: apperr.ErrUnavailable,
			says:    "has not appeared",
		},
		{
			name: "a transaction the network rejected",
			ledger: fakeLedger{payment: payments.LedgerPayment{
				Found: true, Status: "INSUFFICIENT_ACCOUNT_BALANCE",
			}},
			wantErr: apperr.ErrForbidden,
			says:    "INSUFFICIENT_ACCOUNT_BALANCE",
		},
		{
			name: "a payment that went to somebody else",
			ledger: func() payments.Ledger {
				record := settled()
				record.Paid = func(string, int64) bool { return false }
				return fakeLedger{payment: record}
			}(),
			wantErr: apperr.ErrForbidden,
			says:    "did not pay",
		},
		{
			name: "a payment from an account that is not the payer",
			ledger: func() payments.Ledger {
				record := settled()
				record.PaidBy = func(account string) bool { return account == "0.0.999999" }
				return fakeLedger{payment: record}
			}(),
			wantErr: apperr.ErrForbidden,
			says:    "was not paid by",
		},
		{
			name: "a payment bound to a different quote",
			ledger: func() payments.Ledger {
				record := settled()
				record.Memo = "some other nonce"
				return fakeLedger{payment: record}
			}(),
			wantErr: apperr.ErrForbidden,
			says:    "not bound to this quote",
		},
		{
			name: "an old transfer dressed up as this one",
			ledger: func() payments.Ledger {
				record := settled()
				record.ConfirmedAt = chainNow.Add(-time.Hour)
				return fakeLedger{payment: record}
			}(),
			wantErr: apperr.ErrForbidden,
			says:    "long before this quote was issued",
		},
		{
			name:    "a proof with no transaction at all",
			ledger:  fakeLedger{payment: settled()},
			mutate:  func(p *payments.Payment) { p.TxID = "  " },
			wantErr: apperr.ErrValidation,
			says:    "must name the transaction",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := verify(t, tc.ledger, tc.mutate)
			require.ErrorIs(t, err, tc.wantErr)
			assert.Contains(t, err.Error(), tc.says)
		})
	}
}

/*
 * TestAnUnreachableNetworkIsNotARefusal. A mirror that cannot be reached says nothing about
 * whether the machine paid, and answering "your payment is bad" would be the platform
 * blaming a client for its own outage.
 */
func TestAnUnreachableNetworkIsNotARefusal(t *testing.T) {
	t.Parallel()

	err := verify(t, fakeLedger{err: errors.New("dial tcp: connection refused")}, nil)

	require.ErrorIs(t, err, apperr.ErrUnavailable)
	assert.NotContains(t, err.Error(), "forbidden")
}

/*
 * TestTwoClocksDoNotRefuseAnHonestPayment. The quote is timed by this platform and the
 * payment by the network's consensus, and those clocks are never exactly aligned. A strict
 * comparison would refuse a real payment whenever the server ran a minute fast — a failure
 * the customer cannot diagnose, cannot fix, and has already paid for. The nonce in the memo
 * is what actually binds a payment to a quote; this check only catches the ancient.
 */
func TestTwoClocksDoNotRefuseAnHonestPayment(t *testing.T) {
	t.Parallel()

	record := settled()
	record.ConfirmedAt = chainNow.Add(-2 * time.Minute) // the network is a little behind

	require.NoError(t, verify(t, fakeLedger{payment: record}, nil))
}

// TestAnExpiredQuoteIsNotAPrice: a price is a price for as long as it was quoted for, and
// the network is not asked about a payment for a quote that has run out.
func TestAnExpiredQuoteIsNotAPrice(t *testing.T) {
	t.Parallel()

	late := payments.NewChainFacilitator(fakeLedger{payment: settled()}, platformAccount,
		func() time.Time { return chainNow.Add(time.Hour) })

	err := late.Verify(context.Background(), chainQuote(t), payments.Payment{
		Nonce: "7f3a9c21", Payer: agentAccount, TxID: "0.0.10456604@1789039541.207925088",
	})

	require.ErrorIs(t, err, apperr.ErrForbidden)
	assert.Contains(t, err.Error(), "expired")
}
