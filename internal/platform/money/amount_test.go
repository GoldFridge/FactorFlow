package money_test

import (
	"math"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoldFridge/factorflow/internal/platform/money"
)

func TestParse(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		input     string
		currency  money.Currency
		wantMinor int64
		wantErr   error
	}{
		{name: "two decimals", input: "9721.84", currency: money.USD, wantMinor: 972184},
		{name: "no decimals", input: "10000", currency: money.USD, wantMinor: 1000000},
		{name: "one decimal", input: "10.5", currency: money.USD, wantMinor: 1050},
		{name: "trailing zeros are exact", input: "10.50", currency: money.USD, wantMinor: 1050},
		{name: "negative", input: "-0.01", currency: money.USD, wantMinor: -1},
		{name: "zero exponent currency", input: "12345", currency: money.JPY, wantMinor: 12345},
		{name: "explicit plus", input: "+1.00", currency: money.USD, wantMinor: 100},

		{name: "sub-minor precision", input: "9721.845", currency: money.USD, wantErr: money.ErrPrecisionLoss},
		{name: "fraction of a yen", input: "12.5", currency: money.JPY, wantErr: money.ErrPrecisionLoss},
		{name: "not a number", input: "1,00", currency: money.USD, wantErr: money.ErrSyntax},
		{name: "empty", input: "", currency: money.USD, wantErr: money.ErrSyntax},
		{name: "unknown currency", input: "1.00", currency: money.Currency("XYZ"), wantErr: money.ErrInvalidCurrency},
		{name: "beyond int64", input: "92233720368547758.08", currency: money.USD, wantErr: money.ErrOverflow},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := money.Parse(tc.input, tc.currency)
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantMinor, got.Minor())
			assert.Equal(t, tc.currency, got.Currency())
		})
	}
}

func TestAmountString(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		minor int64
		curr  money.Currency
		want  string
	}{
		{name: "usd pads to two decimals", minor: 972184, curr: money.USD, want: "9721.84"},
		{name: "usd zero", minor: 0, curr: money.USD, want: "0.00"},
		{name: "usd sub-unit", minor: 5, curr: money.USD, want: "0.05"},
		{name: "usd negative", minor: -1, curr: money.USD, want: "-0.01"},
		{name: "jpy has no decimals", minor: 12345, curr: money.JPY, want: "12345"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, money.MustNew(tc.minor, tc.curr).String())
		})
	}
}

func TestZeroValueAmountIsInvalid(t *testing.T) {
	t.Parallel()

	var zero money.Amount
	assert.False(t, zero.IsValid())
	assert.Equal(t, "<invalid amount>", zero.String())

	_, err := zero.Add(money.MustParse("1.00", money.USD))
	require.ErrorIs(t, err, money.ErrInvalidCurrency)

	_, err = money.MustParse("1.00", money.USD).Add(zero)
	require.ErrorIs(t, err, money.ErrCurrencyMismatch)
}

func TestAddAndSub(t *testing.T) {
	t.Parallel()

	a := money.MustParse("10000.00", money.USD)
	b := money.MustParse("278.16", money.USD)

	sum, err := a.Add(b)
	require.NoError(t, err)
	assert.Equal(t, "10278.16", sum.String())

	diff, err := a.Sub(b)
	require.NoError(t, err)
	assert.Equal(t, "9721.84", diff.String())

	below, err := b.Sub(a)
	require.NoError(t, err)
	assert.Equal(t, "-9721.84", below.String())
}

func TestCurrencyMismatchIsRejected(t *testing.T) {
	t.Parallel()

	usd := money.MustParse("1.00", money.USD)
	eur := money.MustParse("1.00", money.EUR)

	_, err := usd.Add(eur)
	require.ErrorIs(t, err, money.ErrCurrencyMismatch)

	_, err = usd.Sub(eur)
	require.ErrorIs(t, err, money.ErrCurrencyMismatch)

	_, err = usd.Cmp(eur)
	require.ErrorIs(t, err, money.ErrCurrencyMismatch)

	_, err = money.Sum(usd, eur)
	require.ErrorIs(t, err, money.ErrCurrencyMismatch)

	assert.False(t, usd.Equal(eur))
}

func TestOverflowIsReported(t *testing.T) {
	t.Parallel()

	max := money.MustNew(math.MaxInt64, money.USD)
	min := money.MustNew(math.MinInt64, money.USD)
	one := money.MustNew(1, money.USD)

	_, err := max.Add(one)
	require.ErrorIs(t, err, money.ErrOverflow)

	_, err = min.Sub(one)
	require.ErrorIs(t, err, money.ErrOverflow)

	_, err = min.Neg()
	require.ErrorIs(t, err, money.ErrOverflow)

	_, err = min.Abs()
	require.ErrorIs(t, err, money.ErrOverflow)

	_, err = max.Sub(min)
	require.ErrorIs(t, err, money.ErrOverflow)

	_, err = max.MulInt(2)
	require.ErrorIs(t, err, money.ErrOverflow)
}

func TestMulUsesBankersRounding(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		amount string
		rate   string
		want   string
	}{
		{name: "exact", amount: "10000.00", rate: "0.0640", want: "640.00"},
		{name: "half rounds down to even", amount: "100.00", rate: "0.12345", want: "12.34"},
		{name: "half rounds up to even", amount: "100.00", rate: "0.12355", want: "12.36"},
		{name: "below half rounds down", amount: "100.00", rate: "0.123449", want: "12.34"},
		{name: "above half rounds up", amount: "100.00", rate: "0.123451", want: "12.35"},
		{name: "negative half to even", amount: "-100.00", rate: "0.12345", want: "-12.34"},
		{name: "zero rate", amount: "9721.84", rate: "0", want: "0.00"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := money.MustParse(tc.amount, money.USD).Mul(money.MustParseRate(tc.rate))
			require.NoError(t, err)
			assert.Equal(t, tc.want, got.String())
		})
	}
}

func TestDiv(t *testing.T) {
	t.Parallel()

	// The reserve price formula divides face value by (1 + discount * days / 365).
	face := money.MustParse("10000.00", money.USD)

	got, err := face.Div(money.MustParseRate("1.0625"))
	require.NoError(t, err)
	assert.Equal(t, "9411.76", got.String())

	got, err = face.Div(money.MustParseRate("3"))
	require.NoError(t, err)
	assert.Equal(t, "3333.33", got.String())

	_, err = face.Div(money.ZeroRate())
	require.ErrorIs(t, err, money.ErrDivideByZero)
}

func TestRateAgainst(t *testing.T) {
	t.Parallel()

	part := money.MustParse("2500.00", money.USD)
	whole := money.MustParse("10000.00", money.USD)

	share, err := part.RateAgainst(whole)
	require.NoError(t, err)
	assert.True(t, share.Equal(money.MustParseRate("0.25")), "got %s", share)

	_, err = part.RateAgainst(money.Zero(money.USD))
	require.ErrorIs(t, err, money.ErrDivideByZero)

	_, err = part.RateAgainst(money.MustParse("1.00", money.EUR))
	require.ErrorIs(t, err, money.ErrCurrencyMismatch)
}

func TestSumMinMax(t *testing.T) {
	t.Parallel()

	a := money.MustParse("100.00", money.USD)
	b := money.MustParse("250.50", money.USD)
	c := money.MustParse("-50.50", money.USD)

	total, err := money.Sum(a, b, c)
	require.NoError(t, err)
	assert.Equal(t, "300.00", total.String())

	single, err := money.Sum(a)
	require.NoError(t, err)
	assert.True(t, single.Equal(a))

	_, err = money.Sum()
	require.ErrorIs(t, err, money.ErrInvalidCurrency)

	lo, err := money.Min(a, b)
	require.NoError(t, err)
	assert.True(t, lo.Equal(a))

	hi, err := money.Max(a, b)
	require.NoError(t, err)
	assert.True(t, hi.Equal(b))
}

func TestFromDecimal(t *testing.T) {
	t.Parallel()

	exact, err := money.FromDecimal(decimal.RequireFromString("9721.84"), money.USD)
	require.NoError(t, err)
	assert.Equal(t, int64(972184), exact.Minor())

	_, err = money.FromDecimal(decimal.RequireFromString("9721.8449"), money.USD)
	require.ErrorIs(t, err, money.ErrPrecisionLoss)

	rounded, err := money.FromDecimalRounded(decimal.RequireFromString("9721.8449"), money.USD)
	require.NoError(t, err)
	assert.Equal(t, "9721.84", rounded.String())

	// Half-way values round to even in both directions.
	up, err := money.FromDecimalRounded(decimal.RequireFromString("0.015"), money.USD)
	require.NoError(t, err)
	assert.Equal(t, "0.02", up.String())

	down, err := money.FromDecimalRounded(decimal.RequireFromString("0.025"), money.USD)
	require.NoError(t, err)
	assert.Equal(t, "0.02", down.String())
}

func TestComparisons(t *testing.T) {
	t.Parallel()

	small := money.MustParse("1.00", money.USD)
	large := money.MustParse("2.00", money.USD)

	cmp, err := small.Cmp(large)
	require.NoError(t, err)
	assert.Equal(t, -1, cmp)

	cmp, err = large.Cmp(small)
	require.NoError(t, err)
	assert.Equal(t, 1, cmp)

	cmp, err = small.Cmp(small)
	require.NoError(t, err)
	assert.Equal(t, 0, cmp)

	assert.True(t, small.IsPositive())
	assert.False(t, small.IsNegative())
	assert.True(t, money.Zero(money.USD).IsZero())

	negated, err := small.Neg()
	require.NoError(t, err)
	assert.True(t, negated.IsNegative())

	abs, err := negated.Abs()
	require.NoError(t, err)
	assert.True(t, abs.Equal(small))
}

// FuzzParseStringRoundTrip checks that formatting an amount and parsing it back is the
// identity, so an amount can cross an API boundary as a decimal string without drift.
func FuzzParseStringRoundTrip(f *testing.F) {
	for _, seed := range []int64{0, 1, -1, 100, 972184, math.MaxInt64, math.MinInt64} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, minor int64) {
		for _, curr := range []money.Currency{money.USD, money.JPY} {
			original := money.MustNew(minor, curr)

			reparsed, err := money.Parse(original.String(), curr)
			require.NoErrorf(t, err, "reparsing %s %s", original, curr)
			require.Truef(t, original.Equal(reparsed), "%s %s round-tripped to %s", original, curr, reparsed)
		}
	})
}

// FuzzAddSubInverse checks the ledger invariant that adding and removing the same amount
// leaves the balance untouched whenever neither step overflows.
func FuzzAddSubInverse(f *testing.F) {
	f.Add(int64(0), int64(0))
	f.Add(int64(972184), int64(27816))
	f.Add(int64(-1), int64(1))

	f.Fuzz(func(t *testing.T, aMinor, bMinor int64) {
		a := money.MustNew(aMinor, money.USD)
		b := money.MustNew(bMinor, money.USD)

		sum, err := a.Add(b)
		if err != nil {
			return // Overflow is reported, not silently wrapped; that is checked elsewhere.
		}
		back, err := sum.Sub(b)
		require.NoError(t, err)
		require.Truef(t, a.Equal(back), "(%s + %s) - %s = %s", a, b, b, back)

		commuted, err := b.Add(a)
		require.NoError(t, err)
		require.Truef(t, sum.Equal(commuted), "%s + %s != %s + %s", a, b, b, a)
	})
}

func TestDivFloorNeverRoundsUp(t *testing.T) {
	t.Parallel()

	// Converting cash back into notional at a unit price below 1 must round down, or the
	// allocations of one lot could sum to more than the lot's supply.
	cash := money.MustParse("2438.83", money.USD)
	unitPrice := money.MustParseRate("0.975532")

	floored, err := cash.DivFloor(unitPrice)
	require.NoError(t, err)
	assert.Equal(t, "2500.00", floored.String())

	rounded, err := money.MustParse("100.00", money.USD).Div(money.MustParseRate("3"))
	require.NoError(t, err)
	assert.Equal(t, "33.33", rounded.String())

	floored, err = money.MustParse("100.00", money.USD).DivFloor(money.MustParseRate("3"))
	require.NoError(t, err)
	assert.Equal(t, "33.33", floored.String())

	// 100 / 0.7 is 142.857...; Div rounds to 142.86, DivFloor must stay below.
	rounded, err = money.MustParse("100.00", money.USD).Div(money.MustParseRate("0.7"))
	require.NoError(t, err)
	assert.Equal(t, "142.86", rounded.String())

	floored, err = money.MustParse("100.00", money.USD).DivFloor(money.MustParseRate("0.7"))
	require.NoError(t, err)
	assert.Equal(t, "142.85", floored.String())

	// Negative values floor away from zero, as the name says.
	floored, err = money.MustParse("-100.00", money.USD).DivFloor(money.MustParseRate("0.7"))
	require.NoError(t, err)
	assert.Equal(t, "-142.86", floored.String())

	_, err = cash.DivFloor(money.ZeroRate())
	require.ErrorIs(t, err, money.ErrDivideByZero)

	_, err = money.Amount{}.DivFloor(unitPrice)
	require.ErrorIs(t, err, money.ErrInvalidCurrency)
}
