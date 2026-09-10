package money_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoldFridge/factorflow/internal/platform/money"
)

func TestParseCurrency(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		want    money.Currency
		wantErr bool
	}{
		{name: "exact", input: "USD", want: money.USD},
		{name: "lowercase is normalized", input: "usd", want: money.USD},
		{name: "surrounding space is trimmed", input: "  eur ", want: money.EUR},
		{name: "unknown code", input: "XYZ", wantErr: true},
		{name: "empty", input: "", wantErr: true},
		{name: "numeric code is not supported", input: "840", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := money.ParseCurrency(tc.input)
			if tc.wantErr {
				require.ErrorIs(t, err, money.ErrInvalidCurrency)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestCurrencyExponent(t *testing.T) {
	t.Parallel()

	assert.Equal(t, int32(2), money.USD.Exponent())
	assert.Equal(t, int32(0), money.JPY.Exponent())
	assert.Equal(t, "USD", money.USD.String())

	assert.True(t, money.GBP.IsValid())
	assert.False(t, money.Currency("XYZ").IsValid())
	assert.Panics(t, func() { money.Currency("XYZ").Exponent() })
}

/*
 * TestHbarIsExactInTinybars. A machine paying for an answer pays in what the network moves,
 * and the network moves tinybars. Eight digits is not a preference — a price stored with
 * two would be wrong by a factor of a million, and would look perfectly reasonable.
 */
func TestHbarIsExactInTinybars(t *testing.T) {
	t.Parallel()

	price, err := money.Parse("0.25", money.HBAR)
	require.NoError(t, err)

	assert.Equal(t, int64(25_000_000), price.Minor(), "0.25 HBAR is 25,000,000 tinybars")
	assert.Equal(t, "0.25000000", price.String())
	assert.Equal(t, int32(8), money.HBAR.Exponent())

	// The smallest unit the network has is representable, which is what "exact" has to mean.
	dust, err := money.Parse("0.00000001", money.HBAR)
	require.NoError(t, err)
	assert.Equal(t, int64(1), dust.Minor())

	// And a unit the registry does not know is still refused rather than guessed at.
	_, err = money.ParseCurrency("ETH")
	require.Error(t, err)
}
