package redemption_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoldFridge/factorflow/internal/platform/apperr"
	"github.com/GoldFridge/factorflow/internal/platform/money"
	"github.com/GoldFridge/factorflow/internal/redemption"
)

var testNow = time.Date(2026, time.November, 7, 10, 0, 0, 0, time.UTC)

func params(mutate func(*redemption.NewParams)) redemption.NewParams {
	p := redemption.NewParams{
		ID:         uuid.New(),
		InvoiceID:  uuid.New(),
		Face:       usd("10000.00"),
		Amount:     usd("10000.00"),
		Reference:  "SWIFT-2026-11-07-0042",
		ReceivedAt: testNow.Add(-time.Hour),
		RecordedBy: party(9),
		Holders:    []redemption.Holder{holder(1, "6000.00"), holder(2, "4000.00")},
	}
	if mutate != nil {
		mutate(&p)
	}
	return p
}

// TestRepaymentDividesWhatArrived is the point of the type: a repayment that exists has
// parts that add up to it.
func TestRepaymentDividesWhatArrived(t *testing.T) {
	t.Parallel()

	repayment, err := redemption.New(params(nil), testNow)
	require.NoError(t, err)

	assert.Equal(t, usd("10000.00"), repayment.Amount)
	assert.False(t, repayment.IsShortfall())
	assert.Equal(t, "0.00", repayment.Shortfall().String())

	require.Len(t, repayment.Shares, 2)
	assert.Equal(t, "6000.00", repayment.Shares[0].Amount.String())
	assert.Equal(t, "4000.00", repayment.Shares[1].Amount.String())
	assert.Equal(t, usd("10000.00"), paid(t, repayment.Shares))

	share, ok := repayment.ShareOf(party(2))
	require.True(t, ok)
	assert.Equal(t, "4000.00", share.Amount.String())
	assert.True(t, repayment.Holds(party(1)))
	assert.False(t, repayment.Holds(party(7)), "a stranger holds nothing")
}

/*
 * TestShortPaymentIsStillDivided covers the case the platform exists to handle honestly.
 * A debtor who pays part of what is owed has not made the receivable go away, and the
 * money that did arrive still belongs to the holders in the same proportion.
 */
func TestShortPaymentIsStillDivided(t *testing.T) {
	t.Parallel()

	repayment, err := redemption.New(params(func(p *redemption.NewParams) {
		p.Amount = usd("7500.00")
	}), testNow)
	require.NoError(t, err)

	assert.True(t, repayment.IsShortfall())
	assert.Equal(t, "2500.00", repayment.Shortfall().String())
	assert.Equal(t, usd("7500.00"), paid(t, repayment.Shares))
	assert.Equal(t, "4500.00", repayment.Shares[0].Amount.String())
}

func TestRepaymentRefusesWhatItCannotRecord(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		mutate func(*redemption.NewParams)
	}{
		{"no identity", func(p *redemption.NewParams) { p.ID = uuid.Nil }},
		{"no receivable", func(p *redemption.NewParams) { p.InvoiceID = uuid.Nil }},
		{"nobody recorded it", func(p *redemption.NewParams) { p.RecordedBy = uuid.Nil }},
		{"no reference", func(p *redemption.NewParams) { p.Reference = "   " }},
		{"an unreadable reference", func(p *redemption.NewParams) {
			p.Reference = string(make([]byte, redemption.MaxReferenceLen+1))
		}},
		{"nothing arrived", func(p *redemption.NewParams) { p.Amount = usd("0.00") }},
		{"another currency", func(p *redemption.NewParams) {
			p.Amount = money.MustParse("10000.00", money.EUR)
		}},
		{
			// Refused rather than truncated: nothing here knows whose the surplus is.
			name:   "more than was owed",
			mutate: func(p *redemption.NewParams) { p.Amount = usd("10000.01") },
		},
		{"received in the future", func(p *redemption.NewParams) {
			p.ReceivedAt = testNow.Add(time.Hour)
		}},
		{"received at no time at all", func(p *redemption.NewParams) {
			p.ReceivedAt = time.Time{}
		}},
		{"nobody holds it", func(p *redemption.NewParams) { p.Holders = nil }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := redemption.New(params(tc.mutate), testNow)
			require.ErrorIs(t, err, apperr.ErrValidation)
		})
	}
}
