package risk_test

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoldFridge/factorflow/internal/platform/apperr"
	"github.com/GoldFridge/factorflow/internal/platform/money"
	"github.com/GoldFridge/factorflow/internal/risk"
)

// narrated is an assessment with numbers worth quoting.
func narrated(t *testing.T) *risk.Assessment {
	t.Helper()

	assessment, err := risk.ModelV1().Assess(risk.AssessInput{
		ID:        uuid.New(),
		InvoiceID: uuid.New(),
		Face:      money.MustParse("10000.00", money.USD),
		DaysToDue: 60,
		Features: risk.FeatureVector{
			DSONorm:             money.MustParseRate("0.40"),
			LatePaymentRate:     money.MustParseRate("0.15"),
			DisputeFlag:         money.ZeroRate(),
			DebtorConcentration: money.MustParseRate("0.38"),
			MarketVolatility:    money.MustParseRate("0.12"),
			DebtorRisk:          money.MustParseRate("0.20"),
		},
		Confidence:             money.MustParseRate("0.93"),
		ArithmeticValid:        true,
		Benchmark:              money.MustParseRate("0.0363"),
		LiquidityPremium:       money.MustParseRate("0.0025"),
		MarketSnapshotHash:     "0x" + strings.Repeat("a", 64),
		ConfidentialCommitment: "0x" + strings.Repeat("b", 64),
		ConfidentialNonce:      strings.Repeat("c", 32),
	}, testNow)
	require.NoError(t, err)
	return assessment
}

/*
 * TestNarrationMayNotInventANumber is the control that makes "AI explains, code decides"
 * real rather than declared. A narration that may invent figures is a second, unaccountable
 * pricing path — and the one that sounds like prose is the one people repeat.
 */
func TestNarrationMayNotInventANumber(t *testing.T) {
	t.Parallel()

	assessment := narrated(t)

	_, err := risk.Narrate(assessment, "deepseek-chat", []string{
		"The discount works out at about 9.00% for this receivable.",
	}, testNow)
	require.ErrorIs(t, err, apperr.ErrValidation)
	assert.Contains(t, err.Error(), "9.00")

	// The same sentence with the assessment's own discount is kept.
	kept, err := risk.Narrate(assessment, "deepseek-chat", []string{
		"The discount works out at " + assessment.DiscountAPR.Mul(money.RateFromInt(100)).
			StringFixed(2) + "% for this receivable.",
	}, testNow)
	require.NoError(t, err)
	assert.Equal(t, risk.SourceModel, kept.Source)
	assert.Equal(t, "deepseek-chat", kept.Model)
}

// TestNarrationMayQuoteWhatTheAssessmentPublished, in the shapes a writer actually uses.
func TestNarrationMayQuoteWhatTheAssessmentPublished(t *testing.T) {
	t.Parallel()

	assessment := narrated(t)

	explanation, err := risk.Narrate(assessment, "deepseek-chat", []string{
		"- Grade " + assessment.Grade.String() + " on a default probability of " +
			assessment.PD.Mul(money.RateFromInt(100)).StringFixed(2) + "%.",
		"• A loss given default of " + assessment.LGD.StringFixed(2) + " is the model's base.",
		"The reserve price is " + assessment.ReservePrice.String() + ".",
	}, testNow)
	require.NoError(t, err)

	require.Len(t, explanation.Bullets, 3)
	assert.False(t, strings.HasPrefix(explanation.Bullets[0], "-"), "the list marker is dropped")
	assert.False(t, strings.HasPrefix(explanation.Bullets[1], "•"))
	assert.Equal(t, assessment.ID, explanation.AssessmentID)
}

func TestNarrationRefusesWhatNobodyWouldRead(t *testing.T) {
	t.Parallel()

	assessment := narrated(t)

	cases := []struct {
		name    string
		bullets []string
	}{
		{"nothing at all", nil},
		{"only whitespace", []string{"  ", "-"}},
		{"too many points", []string{"a", "b", "c", "d", "e", "f"}},
		{"an essay in one point", []string{strings.Repeat("a", risk.MaxBulletLen+1)}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := risk.Narrate(assessment, "deepseek-chat", tc.bullets, testNow)
			require.ErrorIs(t, err, apperr.ErrValidation)
		})
	}
}

/*
 * TestDerivedExplanationStandsOnItsOwn: a reader always gets words, whether or not a model
 * answered, and those words say only what the stored numbers say.
 */
func TestDerivedExplanationStandsOnItsOwn(t *testing.T) {
	t.Parallel()

	assessment := narrated(t)
	derived := risk.Derive(assessment, testNow)

	require.NotNil(t, derived)
	assert.Equal(t, risk.SourceDerived, derived.Source)
	assert.Empty(t, derived.Model, "nothing wrote it but this package")
	assert.NotEmpty(t, derived.Bullets)

	joined := strings.Join(derived.Bullets, " ")
	assert.Contains(t, joined, assessment.ReservePrice.String())
	assert.Contains(t, joined, assessment.Grade.String())

	// It obeys the same rule it enforces on a model: every figure in it is published.
	kept, err := risk.Narrate(assessment, "", derived.Bullets, testNow)
	require.NoError(t, err, "the derived explanation would pass the model's own check")
	assert.Len(t, kept.Bullets, len(derived.Bullets))
}
