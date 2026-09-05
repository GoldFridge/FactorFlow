// Package risk turns the minimal output of the confidential workflow into a reproducible
// credit decision: probability of default, loss given default, expected loss, a risk grade
// and a reserve price.
//
// The split the specification insists on is enforced here. The AI extracts unstructured
// evidence and later explains the result; it never sets a number. Everything in this
// package is versioned deterministic code: the same features, the same market snapshot and
// the same model version always produce the same assessment, byte for byte.
package risk

import (
	"errors"

	"github.com/shopspring/decimal"

	"github.com/skimer2king/factorflow/internal/platform/apperr"
	"github.com/skimer2king/factorflow/internal/platform/money"
)

// FeatureVector is the whitelisted set of risk features the confidential workflow is
// allowed to return. Every feature is normalized to [0,1] inside the TEE, so the model
// coefficients are comparable and no raw document value reaches this package.
type FeatureVector struct {
	// DSONorm is the debtor's days-sales-outstanding relative to the demo cohort.
	DSONorm money.Rate
	// LatePaymentRate is the share of the debtor's past invoices paid late.
	LatePaymentRate money.Rate
	// DisputeFlag is 1 when the document contains a dispute or set-off clause.
	DisputeFlag money.Rate
	// DebtorConcentration is the issuer's exposure to this debtor as a share of its book.
	DebtorConcentration money.Rate
	// MarketVolatility is the volatility proxy taken from the market snapshot.
	MarketVolatility money.Rate
	// DebtorRisk is the debtor's own credit signal.
	DebtorRisk money.Rate
}

// featureRef names a feature for validation and for the contribution breakdown. The order
// is fixed: it is the order contributions are reported and hashed in.
type featureRef struct {
	name  string
	value func(FeatureVector) money.Rate
	coeff func(Coefficients) money.Rate
	// set writes the feature back, so storage can rebuild a vector by the same names it
	// was written under. Without it, a rename would silently load a zeroed feature.
	set func(*FeatureVector, money.Rate)
}

var featureRefs = []featureRef{
	{
		"dso_norm",
		func(f FeatureVector) money.Rate { return f.DSONorm },
		func(c Coefficients) money.Rate { return c.DSONorm },
		func(f *FeatureVector, v money.Rate) { f.DSONorm = v },
	},
	{
		"late_payment_rate",
		func(f FeatureVector) money.Rate { return f.LatePaymentRate },
		func(c Coefficients) money.Rate { return c.LatePaymentRate },
		func(f *FeatureVector, v money.Rate) { f.LatePaymentRate = v },
	},
	{
		"dispute_flag",
		func(f FeatureVector) money.Rate { return f.DisputeFlag },
		func(c Coefficients) money.Rate { return c.DisputeFlag },
		func(f *FeatureVector, v money.Rate) { f.DisputeFlag = v },
	},
	{
		"debtor_concentration",
		func(f FeatureVector) money.Rate { return f.DebtorConcentration },
		func(c Coefficients) money.Rate { return c.DebtorConcentration },
		func(f *FeatureVector, v money.Rate) { f.DebtorConcentration = v },
	},
	{
		"market_volatility",
		func(f FeatureVector) money.Rate { return f.MarketVolatility },
		func(c Coefficients) money.Rate { return c.MarketVolatility },
		func(f *FeatureVector, v money.Rate) { f.MarketVolatility = v },
	},
	{
		"debtor_risk",
		func(f FeatureVector) money.Rate { return f.DebtorRisk },
		func(c Coefficients) money.Rate { return c.DebtorRisk },
		func(f *FeatureVector, v money.Rate) { f.DebtorRisk = v },
	},
}

// FeatureNames lists the features in their canonical order.
func FeatureNames() []string {
	names := make([]string, 0, len(featureRefs))
	for _, ref := range featureRefs {
		names = append(names, ref.name)
	}
	return names
}

// Validate reports every feature outside [0,1]. A feature out of range means the workflow
// and this model disagree about normalization, which must fail closed rather than produce
// a score from an unexpected scale.
func (f FeatureVector) Validate() error {
	var violations []error
	for _, ref := range featureRefs {
		v := ref.value(f)
		if v.IsNegative() || v.Cmp(money.OneRate()) > 0 {
			violations = append(violations, apperr.Invalid(
				"features."+ref.name, "must be normalized to [0,1], got %s", v))
		}
	}
	return errors.Join(violations...)
}

// Contribution is one feature's weighted push on the log-odds of default. Contributions
// are what an explanation is allowed to describe: the AI narrates these numbers, it does
// not produce them.
type Contribution struct {
	Feature string
	Value   money.Rate
	Weight  money.Rate
	// Effect is Weight * Value, the term this feature adds to z.
	Effect money.Rate
}

// contributions computes every feature's effect in canonical order.
func (f FeatureVector) contributions(c Coefficients) []Contribution {
	out := make([]Contribution, 0, len(featureRefs))
	for _, ref := range featureRefs {
		value, weight := ref.value(f), ref.coeff(c)
		out = append(out, Contribution{
			Feature: ref.name,
			Value:   value,
			Weight:  weight,
			Effect:  weight.Mul(value).Quantize(rateScale),
		})
	}
	return out
}

// logOdds is the intercept plus every feature's effect.
func (f FeatureVector) logOdds(c Coefficients) money.Rate {
	z := c.Intercept
	for _, contribution := range f.contributions(c) {
		z = z.Add(contribution.Effect)
	}
	return z
}

// sigmoid computes 1 / (1 + exp(-z)) in exact decimal arithmetic.
//
// float64 is not used anywhere in scoring: two machines must agree on a probability of
// default to the last digit for an assessment to be reproducible from stored inputs.
func sigmoid(z money.Rate) (money.Rate, error) {
	expNegZ, err := z.Decimal().Neg().ExpTaylor(expPrecision)
	if err != nil {
		return money.Rate{}, apperr.Invalid("features", "log-odds %s is outside the model's supported range", z)
	}
	denominator := decimal.NewFromInt(1).Add(expNegZ)
	if denominator.IsZero() {
		return money.Rate{}, apperr.Invalid("features", "log-odds %s produced a degenerate sigmoid", z)
	}
	return money.NewRate(decimal.NewFromInt(1).DivRound(denominator, rateScale)), nil
}
