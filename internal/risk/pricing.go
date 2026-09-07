package risk

import (
	"github.com/GoldFridge/factorflow/internal/platform/apperr"
	"github.com/GoldFridge/factorflow/internal/platform/money"
)

// PricingParams are the published pricing constants:
//
//	discount_rate = benchmark + risk_premium(PD, LGD) + liquidity_premium + concentration_premium
//	reserve_price = face / (1 + discount_rate * days_to_due / day_count) - platform_fee
type PricingParams struct {
	// RiskMultiplier scales the expected loss rate PD*LGD into the risk premium. It is the
	// margin the platform charges over the pure expected loss.
	RiskMultiplier money.Rate
	// ConcentrationWeight turns the normalized debtor concentration into a premium.
	ConcentrationWeight money.Rate
	// PlatformFeeRate is the origination fee charged on face value.
	PlatformFeeRate money.Rate
	// MaxDiscountAPR caps the discount rate. A receivable that would price above the cap
	// is not financeable in the MVP rather than being sold at any price.
	MaxDiscountAPR money.Rate
	// DayCount is the day-count basis of the discount rate.
	DayCount int64
}

// PricingParamsV1 returns the published pricing constants for risk-v1.
func PricingParamsV1() PricingParams {
	return PricingParams{
		RiskMultiplier:      money.MustParseRate("2.5"),
		ConcentrationWeight: money.MustParseRate("0.02"),
		PlatformFeeRate:     money.MustParseRate("0.005"),
		MaxDiscountAPR:      money.MustParseRate("0.60"),
		DayCount:            365,
	}
}

// Premiums are the three spreads added to the market benchmark. They are reported
// separately so a rejected or expensive price can be explained by its parts.
type Premiums struct {
	Risk          money.Rate
	Liquidity     money.Rate
	Concentration money.Rate
}

// Total is the sum of the premiums.
func (p Premiums) Total() money.Rate {
	return p.Risk.Add(p.Liquidity).Add(p.Concentration)
}

// RiskPremium is the expected loss rate PD*LGD scaled by the published multiplier.
func (m Model) RiskPremium(pd, lgd money.Rate) money.Rate {
	return pd.Mul(lgd).Mul(m.Pricing.RiskMultiplier).Quantize(rateScale)
}

// ConcentrationPremium prices the issuer's exposure to a single debtor.
func (m Model) ConcentrationPremium(debtorConcentration money.Rate) money.Rate {
	return debtorConcentration.Mul(m.Pricing.ConcentrationWeight).Quantize(rateScale)
}

// PriceInput is what the pricing step needs beyond the model itself.
type PriceInput struct {
	Face      money.Amount
	DaysToDue int64
	// Benchmark comes from a fresh market snapshot. Pricing without one is refused by the
	// caller: a snapshot older than its TTL is not a benchmark.
	Benchmark money.Rate
	// LiquidityPremium is derived from the same snapshot's eligible liquidity.
	LiquidityPremium    money.Rate
	PD                  money.Rate
	LGD                 money.Rate
	DebtorConcentration money.Rate
}

// Price is the deterministic output of the pricing step.
type Price struct {
	Premiums     Premiums
	DiscountAPR  money.Rate
	PlatformFee  money.Amount
	ReservePrice money.Amount
}

// Price computes the discount rate and the reserve price.
//
// The benchmark is an input, not an assumption: with a different live market snapshot the
// same receivable gets a different reserve price, which is exactly the dependency the demo
// has to show.
func (m Model) Price(in PriceInput) (Price, error) {
	if !in.Face.IsPositive() {
		return Price{}, apperr.Invalid("face", "must be greater than zero")
	}
	if in.DaysToDue <= 0 {
		return Price{}, apperr.Invalid("days_to_due", "must be greater than zero, got %d", in.DaysToDue)
	}
	if in.Benchmark.IsNegative() {
		return Price{}, apperr.Invalid("benchmark_apr", "must not be negative, got %s", in.Benchmark)
	}
	if in.LiquidityPremium.IsNegative() {
		return Price{}, apperr.Invalid("liquidity_premium", "must not be negative, got %s", in.LiquidityPremium)
	}

	premiums := Premiums{
		Risk:          m.RiskPremium(in.PD, in.LGD),
		Liquidity:     in.LiquidityPremium.Quantize(rateScale),
		Concentration: m.ConcentrationPremium(in.DebtorConcentration),
	}

	discount := in.Benchmark.Add(premiums.Total()).Quantize(rateScale)
	if discount.Cmp(m.Pricing.MaxDiscountAPR) > 0 {
		return Price{}, apperr.Invalid("discount_apr",
			"priced at %s, above the published maximum %s", discount, m.Pricing.MaxDiscountAPR)
	}

	tenorFraction, err := money.RateFromFraction(in.DaysToDue, m.Pricing.DayCount)
	if err != nil {
		return Price{}, err
	}
	divisor := money.OneRate().Add(discount.Mul(tenorFraction))

	presentValue, err := in.Face.Div(divisor)
	if err != nil {
		return Price{}, err
	}

	fee, err := in.Face.Mul(m.Pricing.PlatformFeeRate)
	if err != nil {
		return Price{}, err
	}

	reserve, err := presentValue.Sub(fee)
	if err != nil {
		return Price{}, err
	}
	if !reserve.IsPositive() {
		return Price{}, apperr.Invalid("reserve_price",
			"discounting leaves no financeable price: %s", reserve)
	}

	return Price{
		Premiums:     premiums,
		DiscountAPR:  discount,
		PlatformFee:  fee,
		ReservePrice: reserve,
	}, nil
}
