package risk_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoldFridge/factorflow/internal/platform/apperr"
	"github.com/GoldFridge/factorflow/internal/platform/money"
	"github.com/GoldFridge/factorflow/internal/risk"
)

// referenceFeatures is the calibration anchor documented on ModelV1: a 60-day receivable
// from a debtor with a moderate late-payment history and no dispute clause.
func referenceFeatures() risk.FeatureVector {
	return risk.FeatureVector{
		DSONorm:             money.MustParseRate("0.40"),
		LatePaymentRate:     money.MustParseRate("0.15"),
		DisputeFlag:         money.ZeroRate(),
		DebtorConcentration: money.MustParseRate("0.38"),
		MarketVolatility:    money.MustParseRate("0.25"),
		DebtorRisk:          money.MustParseRate("0.20"),
	}
}

func uniformFeatures(v string) risk.FeatureVector {
	r := money.MustParseRate(v)
	return risk.FeatureVector{
		DSONorm:             r,
		LatePaymentRate:     r,
		DisputeFlag:         r,
		DebtorConcentration: r,
		MarketVolatility:    r,
		DebtorRisk:          r,
	}
}

// TestProbabilityOfDefaultGoldenValues pins the published calibration. A change here is a
// change to the model, and must come with a new model version.
func TestProbabilityOfDefaultGoldenValues(t *testing.T) {
	t.Parallel()

	m := risk.ModelV1()

	tests := []struct {
		name      string
		features  risk.FeatureVector
		wantPD    string
		wantGrade risk.Grade
	}{
		{name: "clean receivable", features: uniformFeatures("0"), wantPD: "0.004070", wantGrade: risk.GradeA},
		{name: "specification reference", features: referenceFeatures(), wantPD: "0.030384", wantGrade: risk.GradeB},
		{name: "worst case is clamped", features: uniformFeatures("1"), wantPD: "0.800000", wantGrade: risk.GradeE},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			pd, err := m.ProbabilityOfDefault(tc.features)
			require.NoError(t, err)
			assert.Equal(t, tc.wantPD, pd.StringFixed(6))
			assert.Equal(t, tc.wantGrade, risk.GradeFromPD(pd))
		})
	}
}

func TestProbabilityOfDefaultIsClampedToPublishedBounds(t *testing.T) {
	t.Parallel()

	m := risk.ModelV1()

	// Push the intercept far in each direction; the bounds must still hold, because a
	// model calibrated on synthetic data may not claim certainty either way.
	safest := m
	safest.Coefficients.Intercept = money.MustParseRate("-50")
	pd, err := safest.ProbabilityOfDefault(uniformFeatures("0"))
	require.NoError(t, err)
	assert.True(t, pd.Equal(m.PDFloor), "got %s", pd)

	riskiest := m
	riskiest.Coefficients.Intercept = money.MustParseRate("50")
	pd, err = riskiest.ProbabilityOfDefault(uniformFeatures("1"))
	require.NoError(t, err)
	assert.True(t, pd.Equal(m.PDCeiling), "got %s", pd)
}

// TestProbabilityOfDefaultIsMonotonic checks the property that matters commercially: more
// risk in any single feature never lowers the probability of default.
func TestProbabilityOfDefaultIsMonotonic(t *testing.T) {
	t.Parallel()

	m := risk.ModelV1()
	base := uniformFeatures("0.20")

	basePD, err := m.ProbabilityOfDefault(base)
	require.NoError(t, err)

	setters := map[string]func(*risk.FeatureVector, money.Rate){
		"dso_norm":             func(f *risk.FeatureVector, v money.Rate) { f.DSONorm = v },
		"late_payment_rate":    func(f *risk.FeatureVector, v money.Rate) { f.LatePaymentRate = v },
		"dispute_flag":         func(f *risk.FeatureVector, v money.Rate) { f.DisputeFlag = v },
		"debtor_concentration": func(f *risk.FeatureVector, v money.Rate) { f.DebtorConcentration = v },
		"market_volatility":    func(f *risk.FeatureVector, v money.Rate) { f.MarketVolatility = v },
		"debtor_risk":          func(f *risk.FeatureVector, v money.Rate) { f.DebtorRisk = v },
	}

	for name, set := range setters {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			worse := base
			set(&worse, money.MustParseRate("0.90"))
			worsePD, err := m.ProbabilityOfDefault(worse)
			require.NoError(t, err)
			assert.Greaterf(t, worsePD.Cmp(basePD), 0, "raising %s lowered PD: %s -> %s", name, basePD, worsePD)

			better := base
			set(&better, money.ZeroRate())
			betterPD, err := m.ProbabilityOfDefault(better)
			require.NoError(t, err)
			assert.Lessf(t, betterPD.Cmp(basePD), 0, "lowering %s raised PD: %s -> %s", name, basePD, betterPD)
		})
	}
}

func TestProbabilityOfDefaultIsDeterministic(t *testing.T) {
	t.Parallel()

	m := risk.ModelV1()
	first, err := m.ProbabilityOfDefault(referenceFeatures())
	require.NoError(t, err)

	for range 50 {
		again, err := m.ProbabilityOfDefault(referenceFeatures())
		require.NoError(t, err)
		require.Equal(t, first.String(), again.String())
	}
}

func TestFeaturesOutsideUnitRangeAreRejected(t *testing.T) {
	t.Parallel()

	m := risk.ModelV1()

	features := referenceFeatures()
	features.DSONorm = money.MustParseRate("1.4")
	features.DebtorRisk = money.MustParseRate("-0.1")

	_, err := m.ProbabilityOfDefault(features)
	require.ErrorIs(t, err, apperr.ErrValidation)

	fields := apperr.Fields(err)
	require.Len(t, fields, 2, "every out-of-range feature is reported")
	assert.Equal(t, "features.dso_norm", fields[0].Field)
	assert.Equal(t, "features.debtor_risk", fields[1].Field)
}

func TestLossGivenDefault(t *testing.T) {
	t.Parallel()

	m := risk.ModelV1()

	tests := []struct {
		name string
		mit  risk.Mitigations
		want string
	}{
		{name: "unsecured uses the base", mit: risk.Mitigations{}, want: "0.450000"},
		{name: "recourse", mit: risk.Mitigations{Recourse: true}, want: "0.300000"},
		{name: "collateral", mit: risk.Mitigations{Collateralized: true}, want: "0.350000"},
		{name: "both", mit: risk.Mitigations{Recourse: true, Collateralized: true}, want: "0.200000"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, m.LossGivenDefault(tc.mit).StringFixed(6))
		})
	}
}

func TestLossGivenDefaultIsClamped(t *testing.T) {
	t.Parallel()

	m := risk.ModelV1()
	m.LGDRecourseRelief = money.MustParseRate("0.50")
	m.LGDCollateralRelief = money.MustParseRate("0.50")
	assert.True(t, m.LossGivenDefault(risk.Mitigations{Recourse: true, Collateralized: true}).Equal(m.LGDFloor))

	m = risk.ModelV1()
	m.LGDBase = money.MustParseRate("0.99")
	assert.True(t, m.LossGivenDefault(risk.Mitigations{}).Equal(m.LGDCeiling))
}

func TestExpectedLoss(t *testing.T) {
	t.Parallel()

	m := risk.ModelV1()
	face := money.MustParse("10000.00", money.USD)

	// The specification's worked example: PD 3.10%, LGD 45%, face 10000 gives 139.50.
	el, err := m.ExpectedLoss(money.MustParseRate("0.0310"), money.MustParseRate("0.45"), face)
	require.NoError(t, err)
	assert.Equal(t, "139.50", el.String())

	_, err = m.ExpectedLoss(money.MustParseRate("0.0310"), money.MustParseRate("0.45"), money.Zero(money.USD))
	require.ErrorIs(t, err, apperr.ErrValidation)
}

func TestConfidenceGate(t *testing.T) {
	t.Parallel()

	m := risk.ModelV1()

	assert.False(t, m.RequiresManualReview(money.MustParseRate("0.92"), true))
	assert.False(t, m.RequiresManualReview(money.MustParseRate("0.85"), true), "the gate is inclusive at the bound")
	assert.True(t, m.RequiresManualReview(money.MustParseRate("0.84"), true))
	assert.True(t, m.RequiresManualReview(money.MustParseRate("0.99"), false), "failed arithmetic always blocks")
}

func TestFeatureNamesAreCanonical(t *testing.T) {
	t.Parallel()

	assert.Equal(t, []string{
		"dso_norm",
		"late_payment_rate",
		"dispute_flag",
		"debtor_concentration",
		"market_volatility",
		"debtor_risk",
	}, risk.FeatureNames())
}
