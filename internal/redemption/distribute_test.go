package redemption_test

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoldFridge/factorflow/internal/platform/apperr"
	"github.com/GoldFridge/factorflow/internal/platform/money"
	"github.com/GoldFridge/factorflow/internal/redemption"
)

func usd(s string) money.Amount { return money.MustParse(s, money.USD) }

// holder is a party with a stake, named by a fixed id so a division is reproducible.
func holder(n byte, notional string) redemption.Holder {
	return redemption.Holder{PartyID: party(n), Notional: usd(notional)}
}

func party(n byte) uuid.UUID {
	return uuid.UUID{0x11, 0, 0, 0, 0, 0, 0x40, 0, 0x80, 0, 0, 0, 0, 0, 0, n}
}

// paid sums what a division handed out, which is the only number that has to match.
func paid(t *testing.T, shares []redemption.Share) money.Amount {
	t.Helper()

	amounts := make([]money.Amount, 0, len(shares))
	for _, share := range shares {
		amounts = append(amounts, share.Amount)
	}
	total, err := money.Sum(amounts...)
	require.NoError(t, err)
	return total
}

// TestDivisionIsExact is the rule: what arrived is what is handed out, to the minor unit.
func TestDivisionIsExact(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		received string
		holders  []redemption.Holder
		want     []string
	}{
		{
			name:     "an even split stays even",
			received: "10000.00",
			holders:  []redemption.Holder{holder(1, "5000.00"), holder(2, "5000.00")},
			want:     []string{"5000.00", "5000.00"},
		},
		{
			name:     "a third each, and the odd cent goes somewhere",
			received: "10.00",
			holders:  []redemption.Holder{holder(1, "1.00"), holder(2, "1.00"), holder(3, "1.00")},
			want:     []string{"3.34", "3.33", "3.33"},
		},
		{
			name:     "a short payment is divided in the same proportion",
			received: "7500.00",
			holders:  []redemption.Holder{holder(1, "7500.00"), holder(2, "2500.00")},
			want:     []string{"5625.00", "1875.00"},
		},
		{
			name:     "a holder too small to earn a whole unit gets nothing",
			received: "0.02",
			holders:  []redemption.Holder{holder(1, "9999.00"), holder(2, "1.00")},
			want:     []string{"0.02", "0.00"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			shares, err := redemption.Distribute(usd(tc.received), tc.holders)
			require.NoError(t, err)
			require.Len(t, shares, len(tc.holders))

			for i, share := range shares {
				assert.Equal(t, tc.holders[i].PartyID, share.PartyID, "shares keep the caller's order")
				assert.Equal(t, tc.want[i], share.Amount.String())
			}
			assert.Equal(t, usd(tc.received), paid(t, shares), "every unit paid was handed out")
		})
	}
}

/*
 * TestDivisionIsDeterministic is what stops the platform being the party that decides who
 * gets the odd cent. The same payment, described in a different order, has to split the
 * same way — otherwise the answer depends on how the rows came back from a database.
 */
func TestDivisionIsDeterministic(t *testing.T) {
	t.Parallel()

	forward := []redemption.Holder{holder(1, "1.00"), holder(2, "1.00"), holder(3, "1.00")}
	backward := []redemption.Holder{holder(3, "1.00"), holder(2, "1.00"), holder(1, "1.00")}

	first, err := redemption.Distribute(usd("10.00"), forward)
	require.NoError(t, err)
	second, err := redemption.Distribute(usd("10.00"), backward)
	require.NoError(t, err)

	got := map[uuid.UUID]string{}
	for _, share := range second {
		got[share.PartyID] = share.Amount.String()
	}
	for _, share := range first {
		assert.Equal(t, share.Amount.String(), got[share.PartyID],
			"party %s is paid the same either way", share.PartyID)
	}
}

// TestDivisionSurvivesLargeAmounts covers the product that overflows int64 long before
// either number involved is unreasonable.
func TestDivisionSurvivesLargeAmounts(t *testing.T) {
	t.Parallel()

	huge := []redemption.Holder{
		holder(1, "70000000000.00"),
		holder(2, "30000000000.01"),
	}

	shares, err := redemption.Distribute(usd("100000000000.01"), huge)
	require.NoError(t, err)
	assert.Equal(t, usd("100000000000.01"), paid(t, shares))
	assert.Equal(t, "70000000000.00", shares[0].Amount.String())
}

func TestDivisionRefusesWhatItCannotSplit(t *testing.T) {
	t.Parallel()

	one := holder(1, "100.00")

	cases := []struct {
		name     string
		received money.Amount
		holders  []redemption.Holder
	}{
		{"nothing was received", usd("0.00"), []redemption.Holder{one}},
		{"nobody holds it", usd("100.00"), nil},
		{"a holder holds nothing", usd("100.00"), []redemption.Holder{holder(1, "0.00")}},
		{
			// Two rows for one party would each take a share of the whole; the caller
			// aggregates before asking.
			name:     "a party is named twice",
			received: usd("100.00"),
			holders:  []redemption.Holder{one, holder(1, "50.00")},
		},
		{
			name:     "a holding in another currency",
			received: usd("100.00"),
			holders: []redemption.Holder{
				{PartyID: party(2), Notional: money.MustParse("100.00", money.EUR)},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := redemption.Distribute(tc.received, tc.holders)
			require.ErrorIs(t, err, apperr.ErrValidation)
		})
	}
}
