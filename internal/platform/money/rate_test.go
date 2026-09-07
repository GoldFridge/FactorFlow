package money_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoldFridge/factorflow/internal/platform/money"
)

func TestParseRate(t *testing.T) {
	t.Parallel()

	r, err := money.ParseRate("0.0640")
	require.NoError(t, err)
	assert.Equal(t, "0.064", r.String())
	assert.Equal(t, "0.0640", r.StringFixed(4))

	_, err = money.ParseRate("six percent")
	require.ErrorIs(t, err, money.ErrSyntax)
}

func TestRateArithmetic(t *testing.T) {
	t.Parallel()

	benchmark := money.MustParseRate("0.0640")
	riskPremium := money.MustParseRate("0.0450")
	liquidity := money.MustParseRate("0.0125")

	discount := benchmark.Add(riskPremium).Add(liquidity)
	assert.Equal(t, "0.1215", discount.StringFixed(4))

	assert.Equal(t, "0.0190", discount.Sub(money.MustParseRate("0.1025")).StringFixed(4))
	assert.Equal(t, "0.002880", benchmark.Mul(money.MustParseRate("0.045")).StringFixed(6))

	quotient, err := benchmark.Div(money.RateFromInt(2))
	require.NoError(t, err)
	assert.Equal(t, "0.0320", quotient.StringFixed(4))

	_, err = benchmark.Div(money.ZeroRate())
	require.ErrorIs(t, err, money.ErrDivideByZero)

	// The reserve price formula scales the discount by days to maturity.
	tenor, err := money.RateFromFraction(60, 365)
	require.NoError(t, err)
	assert.Equal(t, "0.019973", discount.Mul(tenor).StringFixed(6))
}

func TestRateFromFractionRejectsZeroDenominator(t *testing.T) {
	t.Parallel()

	_, err := money.RateFromFraction(1, 0)
	require.ErrorIs(t, err, money.ErrDivideByZero)

	_, err = money.RateFromInt(1).DivInt(0)
	require.ErrorIs(t, err, money.ErrDivideByZero)

	half, err := money.RateFromFraction(1, 2)
	require.NoError(t, err)
	assert.True(t, half.Equal(money.MustParseRate("0.5")))
}

func TestRateClamp(t *testing.T) {
	t.Parallel()

	// PD is clamped to [0.001, 0.80] by the risk model.
	lo := money.MustParseRate("0.001")
	hi := money.MustParseRate("0.80")

	assert.True(t, money.MustParseRate("0.0001").Clamp(lo, hi).Equal(lo))
	assert.True(t, money.MustParseRate("0.95").Clamp(lo, hi).Equal(hi))
	assert.True(t, money.MustParseRate("0.031").Clamp(lo, hi).Equal(money.MustParseRate("0.031")))
	assert.True(t, lo.Clamp(lo, hi).Equal(lo), "bounds are inclusive")
	assert.True(t, hi.Clamp(lo, hi).Equal(hi), "bounds are inclusive")

	assert.Panics(t, func() { money.ZeroRate().Clamp(hi, lo) }, "inverted bounds are a programming error")
}

func TestRateQuantizeUsesBankersRounding(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "0.1234", money.MustParseRate("0.12345").Quantize(4).StringFixed(4))
	assert.Equal(t, "0.1236", money.MustParseRate("0.12355").Quantize(4).StringFixed(4))
	assert.Equal(t, "0.1235", money.MustParseRate("0.123451").Quantize(4).StringFixed(4))
}

func TestRatePredicatesAndOrdering(t *testing.T) {
	t.Parallel()

	neg := money.MustParseRate("-0.01")
	zero := money.ZeroRate()
	pos := money.MustParseRate("0.01")

	assert.True(t, neg.IsNegative())
	assert.True(t, zero.IsZero())
	assert.True(t, pos.IsPositive())
	assert.True(t, money.OneRate().Equal(money.RateFromInt(1)))

	assert.Equal(t, -1, neg.Cmp(pos))
	assert.Equal(t, 1, pos.Cmp(neg))
	assert.Equal(t, 0, pos.Cmp(money.MustParseRate("0.0100")), "equality ignores scale")

	assert.True(t, money.MinRate(neg, pos).Equal(neg))
	assert.True(t, money.MaxRate(neg, pos).Equal(pos))
	assert.True(t, neg.Neg().Equal(pos))
	assert.True(t, pos.MulInt(3).Equal(money.MustParseRate("0.03")))
}

// FuzzRateAddSubInverse checks that rate arithmetic stays exact: unlike float64, adding and
// removing a premium must return the original rate for any inputs.
func FuzzRateAddSubInverse(f *testing.F) {
	f.Add("0.0640", "0.0450")
	f.Add("0", "0")
	f.Add("-0.1", "0.30000000001")

	f.Fuzz(func(t *testing.T, aLit, bLit string) {
		a, err := money.ParseRate(aLit)
		if err != nil {
			return
		}
		b, err := money.ParseRate(bLit)
		if err != nil {
			return
		}

		require.Truef(t, a.Add(b).Sub(b).Equal(a), "(%s + %s) - %s != %s", a, b, b, a)
		require.Truef(t, a.Add(b).Equal(b.Add(a)), "%s + %s is not commutative", a, b)
	})
}
