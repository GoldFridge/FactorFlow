package risk_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoldFridge/factorflow/internal/platform/apperr"
	"github.com/GoldFridge/factorflow/internal/platform/money"
	"github.com/GoldFridge/factorflow/internal/risk"
)

func referencePriceInput() risk.PriceInput {
	return risk.PriceInput{
		Face:                money.MustParse("10000.00", money.USD),
		DaysToDue:           60,
		Benchmark:           money.MustParseRate("0.0640"),
		LiquidityPremium:    money.MustParseRate("0.0150"),
		PD:                  money.MustParseRate("0.030384"),
		LGD:                 money.MustParseRate("0.45"),
		DebtorConcentration: money.MustParseRate("0.38"),
	}
}

// TestPriceGoldenValues pins the published pricing constants against the reference
// receivable, one component at a time.
func TestPriceGoldenValues(t *testing.T) {
	t.Parallel()

	price, err := risk.ModelV1().Price(referencePriceInput())
	require.NoError(t, err)

	assert.Equal(t, "0.034182", price.Premiums.Risk.StringFixed(6), "PD * LGD * 2.5")
	assert.Equal(t, "0.015000", price.Premiums.Liquidity.StringFixed(6), "taken from the market snapshot")
	assert.Equal(t, "0.007600", price.Premiums.Concentration.StringFixed(6), "concentration * 0.02")
	assert.Equal(t, "0.056782", price.Premiums.Total().StringFixed(6))
	assert.Equal(t, "0.120782", price.DiscountAPR.StringFixed(6), "benchmark plus premiums")
	assert.Equal(t, "50.00", price.PlatformFee.String(), "50 bps of face")
	assert.Equal(t, "9755.32", price.ReservePrice.String())
}

// TestReservePriceFollowsTheBenchmark is the claim the demo has to survive: the live
// market snapshot materially moves the price, it is not decoration.
func TestReservePriceFollowsTheBenchmark(t *testing.T) {
	t.Parallel()

	m := risk.ModelV1()

	cheap, err := m.Price(referencePriceInput())
	require.NoError(t, err)

	dearer := referencePriceInput()
	dearer.Benchmark = money.MustParseRate("0.0900")
	expensive, err := m.Price(dearer)
	require.NoError(t, err)

	assert.Equal(t, "0.146782", expensive.DiscountAPR.StringFixed(6))
	assert.Equal(t, "9714.40", expensive.ReservePrice.String())

	cmp, err := expensive.ReservePrice.Cmp(cheap.ReservePrice)
	require.NoError(t, err)
	assert.Equal(t, -1, cmp, "a higher benchmark must lower the reserve price")
}

func TestReservePriceFallsWithTenor(t *testing.T) {
	t.Parallel()

	m := risk.ModelV1()

	short, err := m.Price(referencePriceInput())
	require.NoError(t, err)

	longerInput := referencePriceInput()
	longerInput.DaysToDue = 120
	longer, err := m.Price(longerInput)
	require.NoError(t, err)

	assert.Equal(t, "9568.07", longer.ReservePrice.String())

	cmp, err := longer.ReservePrice.Cmp(short.ReservePrice)
	require.NoError(t, err)
	assert.Equal(t, -1, cmp, "money further away is worth less today")
}

func TestReservePriceFallsWithRisk(t *testing.T) {
	t.Parallel()

	m := risk.ModelV1()

	safe, err := m.Price(referencePriceInput())
	require.NoError(t, err)

	riskyInput := referencePriceInput()
	riskyInput.PD = money.MustParseRate("0.18")
	risky, err := m.Price(riskyInput)
	require.NoError(t, err)

	cmp, err := risky.ReservePrice.Cmp(safe.ReservePrice)
	require.NoError(t, err)
	assert.Equal(t, -1, cmp, "a riskier receivable must price lower")
}

func TestPriceIsDeterministic(t *testing.T) {
	t.Parallel()

	m := risk.ModelV1()
	first, err := m.Price(referencePriceInput())
	require.NoError(t, err)

	for range 50 {
		again, err := m.Price(referencePriceInput())
		require.NoError(t, err)
		require.Equal(t, first.ReservePrice.String(), again.ReservePrice.String())
		require.Equal(t, first.DiscountAPR.String(), again.DiscountAPR.String())
	}
}

func TestPriceRejectsInvalidInput(t *testing.T) {
	t.Parallel()

	m := risk.ModelV1()

	tests := []struct {
		name      string
		mutate    func(*risk.PriceInput)
		wantField string
	}{
		{name: "zero face", mutate: func(in *risk.PriceInput) { in.Face = money.Zero(money.USD) }, wantField: "face"},
		{name: "negative face", mutate: func(in *risk.PriceInput) { in.Face = money.MustParse("-1.00", money.USD) }, wantField: "face"},
		{name: "already due", mutate: func(in *risk.PriceInput) { in.DaysToDue = 0 }, wantField: "days_to_due"},
		{name: "overdue", mutate: func(in *risk.PriceInput) { in.DaysToDue = -5 }, wantField: "days_to_due"},
		{name: "negative benchmark", mutate: func(in *risk.PriceInput) { in.Benchmark = money.MustParseRate("-0.01") }, wantField: "benchmark_apr"},
		{name: "negative liquidity premium", mutate: func(in *risk.PriceInput) { in.LiquidityPremium = money.MustParseRate("-0.01") }, wantField: "liquidity_premium"},
		{name: "priced above the cap", mutate: func(in *risk.PriceInput) { in.PD = money.MustParseRate("0.75") }, wantField: "discount_apr"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			in := referencePriceInput()
			tc.mutate(&in)

			_, err := m.Price(in)
			require.ErrorIs(t, err, apperr.ErrValidation)
			assert.Contains(t, fieldNames(apperr.Fields(err)), tc.wantField)
		})
	}
}

func TestPriceRefusesWhenFeesLeaveNothing(t *testing.T) {
	t.Parallel()

	m := risk.ModelV1()
	m.Pricing.PlatformFeeRate = money.MustParseRate("2.0")

	_, err := m.Price(referencePriceInput())
	require.ErrorIs(t, err, apperr.ErrValidation)
	assert.Contains(t, fieldNames(apperr.Fields(err)), "reserve_price")
}

func fieldNames(fields []*apperr.FieldError) []string {
	names := make([]string, 0, len(fields))
	for _, f := range fields {
		names = append(names, f.Field)
	}
	return names
}
