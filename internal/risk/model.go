package risk

import (
	"github.com/skimer2king/factorflow/internal/platform/apperr"
	"github.com/skimer2king/factorflow/internal/platform/money"
)

// ModelVersionV1 is the published identifier of the coefficients below. Every assessment
// stores it, so a stored result can always be recomputed with the model that produced it.
const ModelVersionV1 = "risk-v1"

const (
	// rateScale is the number of decimal digits kept on every derived rate. Fixing it is
	// what makes two runs of the model agree exactly rather than approximately.
	rateScale int32 = 6
	// expPrecision is the working precision of the exponential in the sigmoid.
	expPrecision int32 = 18
)

// Coefficients are the published weights of the logistic model:
//
//	z = b0 + b1*DSO_norm + b2*late_payment_rate + b3*dispute_flag
//	      + b4*debtor_concentration + b5*market_volatility + b6*debtor_risk
type Coefficients struct {
	Intercept           money.Rate
	DSONorm             money.Rate
	LatePaymentRate     money.Rate
	DisputeFlag         money.Rate
	DebtorConcentration money.Rate
	MarketVolatility    money.Rate
	DebtorRisk          money.Rate
}

// Mitigations are the credit enhancements that lower loss given default.
type Mitigations struct {
	// Recourse means the issuer remains liable if the debtor does not pay.
	Recourse bool
	// Collateralized means the receivable is backed by pledged assets.
	Collateralized bool
}

// Model is a versioned, fully published risk model.
//
// It is a value, not a service: it holds no clients, no clock and no storage, so scoring
// is a pure function of its inputs. That is what makes an assessment reproducible from the
// record kept in the database.
type Model struct {
	Version      string
	Coefficients Coefficients

	// PDFloor and PDCeiling clamp the probability of default. A synthetic-data model must
	// not claim certainty in either direction.
	PDFloor   money.Rate
	PDCeiling money.Rate

	// LGDBase is loss given default before mitigations; the reliefs are subtracted from it
	// and the result is clamped to [LGDFloor, LGDCeiling].
	LGDBase             money.Rate
	LGDRecourseRelief   money.Rate
	LGDCollateralRelief money.Rate
	LGDFloor            money.Rate
	LGDCeiling          money.Rate

	// MinConfidence is the extraction confidence below which an assessment cannot be
	// auto-approved.
	MinConfidence money.Rate

	Pricing PricingParams
}

// ModelV1 returns the published risk-v1 model.
//
// The coefficients are calibrated on the synthetic demo dataset and are documented rather
// than hidden: they are not a production credit model, and the specification requires them
// to travel with the model version.
//
// Calibration anchors, all with no mitigations:
//   - a clean receivable (every feature at 0) scores PD 0.41%, grade A
//   - the reference receivable of the specification (DSO 0.40, late 0.15, no dispute,
//     concentration 0.38, volatility 0.25, debtor risk 0.20) scores PD 3.04%, grade B
//   - a worst-case receivable (every feature at 1) scores above the ceiling and is
//     clamped to PD 80%, grade E
func ModelV1() Model {
	return Model{
		Version: ModelVersionV1,
		Coefficients: Coefficients{
			Intercept:           money.MustParseRate("-5.5"),
			DSONorm:             money.MustParseRate("1.8"),
			LatePaymentRate:     money.MustParseRate("2.4"),
			DisputeFlag:         money.MustParseRate("1.1"),
			DebtorConcentration: money.MustParseRate("0.9"),
			MarketVolatility:    money.MustParseRate("0.7"),
			DebtorRisk:          money.MustParseRate("2.2"),
		},
		PDFloor:             money.MustParseRate("0.001"),
		PDCeiling:           money.MustParseRate("0.80"),
		LGDBase:             money.MustParseRate("0.45"),
		LGDRecourseRelief:   money.MustParseRate("0.15"),
		LGDCollateralRelief: money.MustParseRate("0.10"),
		LGDFloor:            money.MustParseRate("0.10"),
		LGDCeiling:          money.MustParseRate("0.90"),
		MinConfidence:       money.MustParseRate("0.85"),
		Pricing:             PricingParamsV1(),
	}
}

// ProbabilityOfDefault computes PD from the feature vector, clamped to the published
// bounds.
func (m Model) ProbabilityOfDefault(f FeatureVector) (money.Rate, error) {
	if err := f.Validate(); err != nil {
		return money.Rate{}, err
	}
	pd, err := sigmoid(f.logOdds(m.Coefficients))
	if err != nil {
		return money.Rate{}, err
	}
	return pd.Quantize(rateScale).Clamp(m.PDFloor, m.PDCeiling), nil
}

// LossGivenDefault applies the mitigations to the base LGD and clamps the result.
func (m Model) LossGivenDefault(mit Mitigations) money.Rate {
	lgd := m.LGDBase
	if mit.Recourse {
		lgd = lgd.Sub(m.LGDRecourseRelief)
	}
	if mit.Collateralized {
		lgd = lgd.Sub(m.LGDCollateralRelief)
	}
	return lgd.Quantize(rateScale).Clamp(m.LGDFloor, m.LGDCeiling)
}

// ExpectedLoss is PD * LGD * EAD, where exposure at default is the invoice face value.
func (m Model) ExpectedLoss(pd, lgd money.Rate, ead money.Amount) (money.Amount, error) {
	if !ead.IsPositive() {
		return money.Amount{}, apperr.Invalid("face", "exposure at default must be greater than zero")
	}
	return ead.Mul(pd.Mul(lgd))
}

// RequiresManualReview reports whether the confidence gate blocks auto-approval: the
// specification refuses to auto-approve below 0.85 confidence or when the workflow's
// arithmetic checks failed.
func (m Model) RequiresManualReview(confidence money.Rate, arithmeticValid bool) bool {
	return !arithmeticValid || confidence.Cmp(m.MinConfidence) < 0
}
