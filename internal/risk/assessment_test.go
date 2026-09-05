package risk_test

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/skimer2king/factorflow/internal/platform/apperr"
	"github.com/skimer2king/factorflow/internal/platform/money"
	"github.com/skimer2king/factorflow/internal/risk"
)

var testNow = time.Date(2026, time.September, 5, 12, 0, 0, 0, time.UTC)

const (
	testSnapshotHash = "0x" + "3d2f1c0b9a8e7d6c5b4a39281706f5e4d3c2b1a09f8e7d6c5b4a39281706f5e4"
	testCommitment   = "0x" + "1a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5d6e7f809"
)

func referenceAssessInput() risk.AssessInput {
	return risk.AssessInput{
		ID:                     uuid.New(),
		InvoiceID:              uuid.New(),
		Face:                   money.MustParse("10000.00", money.USD),
		DaysToDue:              60,
		Features:               referenceFeatures(),
		Confidence:             money.MustParseRate("0.93"),
		ArithmeticValid:        true,
		Benchmark:              money.MustParseRate("0.0640"),
		LiquidityPremium:       money.MustParseRate("0.0150"),
		MarketSnapshotHash:     testSnapshotHash,
		ConfidentialCommitment: testCommitment,
	}
}

func TestAssessProducesTheFullRecord(t *testing.T) {
	t.Parallel()

	in := referenceAssessInput()
	got, err := risk.ModelV1().Assess(in, testNow)
	require.NoError(t, err)

	assert.Equal(t, in.ID, got.ID)
	assert.Equal(t, in.InvoiceID, got.InvoiceID)
	assert.Equal(t, risk.ModelVersionV1, got.ModelVersion)

	assert.Equal(t, "0.030384", got.PD.StringFixed(6))
	assert.Equal(t, "0.450000", got.LGD.StringFixed(6))
	assert.Equal(t, "136.73", got.ExpectedLoss.String())
	assert.Equal(t, risk.GradeB, got.Grade)

	assert.Equal(t, "0.064000", got.BenchmarkAPR.StringFixed(6))
	assert.Equal(t, "0.120782", got.DiscountAPR.StringFixed(6))
	assert.Equal(t, "50.00", got.PlatformFee.String())
	assert.Equal(t, "9755.32", got.ReservePrice.String())

	assert.Equal(t, testSnapshotHash, got.MarketSnapshotHash, "the price is bound to its snapshot")
	assert.Equal(t, testCommitment, got.ConfidentialCommitment)
	assert.False(t, got.RequiresManualReview)
	assert.Equal(t, testNow, got.CreatedAt)
	assert.Len(t, got.Contributions, len(risk.FeatureNames()))
}

// TestAssessIsReproducible is the acceptance criterion that a judge can recompute a
// published price from the stored inputs and the model version.
func TestAssessIsReproducible(t *testing.T) {
	t.Parallel()

	m := risk.ModelV1()
	in := referenceAssessInput()

	first, err := m.Assess(in, testNow)
	require.NoError(t, err)

	for range 20 {
		again, err := m.Assess(in, testNow.Add(time.Hour))
		require.NoError(t, err)

		assert.Equal(t, first.PD.String(), again.PD.String())
		assert.Equal(t, first.LGD.String(), again.LGD.String())
		assert.Equal(t, first.ExpectedLoss.String(), again.ExpectedLoss.String())
		assert.Equal(t, first.DiscountAPR.String(), again.DiscountAPR.String())
		assert.Equal(t, first.ReservePrice.String(), again.ReservePrice.String())
		assert.Equal(t, first.Grade, again.Grade)
	}
}

func TestAssessAppliesTheConfidenceGate(t *testing.T) {
	t.Parallel()

	m := risk.ModelV1()

	lowConfidence := referenceAssessInput()
	lowConfidence.Confidence = money.MustParseRate("0.62")
	got, err := m.Assess(lowConfidence, testNow)
	require.NoError(t, err)
	assert.True(t, got.RequiresManualReview, "low extraction confidence blocks auto-approval")

	badArithmetic := referenceAssessInput()
	badArithmetic.ArithmeticValid = false
	got, err = m.Assess(badArithmetic, testNow)
	require.NoError(t, err)
	assert.True(t, got.RequiresManualReview, "failed line-item arithmetic blocks auto-approval")
}

func TestAssessRejectsInvalidInput(t *testing.T) {
	t.Parallel()

	m := risk.ModelV1()

	tests := []struct {
		name      string
		mutate    func(*risk.AssessInput)
		wantField string
	}{
		{name: "nil id", mutate: func(in *risk.AssessInput) { in.ID = uuid.Nil }, wantField: "id"},
		{name: "nil invoice", mutate: func(in *risk.AssessInput) { in.InvoiceID = uuid.Nil }, wantField: "invoice_id"},
		{name: "confidence above one", mutate: func(in *risk.AssessInput) { in.Confidence = money.MustParseRate("1.2") }, wantField: "confidence"},
		{name: "negative confidence", mutate: func(in *risk.AssessInput) { in.Confidence = money.MustParseRate("-0.1") }, wantField: "confidence"},
		{name: "snapshot hash without prefix", mutate: func(in *risk.AssessInput) {
			in.MarketSnapshotHash = strings.TrimPrefix(testSnapshotHash, "0x")
		}, wantField: "market_snapshot_hash"},
		{name: "snapshot hash too short", mutate: func(in *risk.AssessInput) { in.MarketSnapshotHash = "0xdeadbeef" }, wantField: "market_snapshot_hash"},
		{name: "missing commitment", mutate: func(in *risk.AssessInput) { in.ConfidentialCommitment = "" }, wantField: "confidential_commitment"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			in := referenceAssessInput()
			tc.mutate(&in)

			got, err := m.Assess(in, testNow)
			require.Nil(t, got)
			require.ErrorIs(t, err, apperr.ErrValidation)
			assert.Contains(t, fieldNames(apperr.Fields(err)), tc.wantField)
		})
	}
}

func TestAssessNormalizesDigestCase(t *testing.T) {
	t.Parallel()

	in := referenceAssessInput()
	in.MarketSnapshotHash = strings.ToUpper(testSnapshotHash)
	in.ConfidentialCommitment = strings.ToUpper(testCommitment)

	got, err := risk.ModelV1().Assess(in, testNow)
	require.NoError(t, err)
	assert.Equal(t, testSnapshotHash, got.MarketSnapshotHash)
	assert.Equal(t, testCommitment, got.ConfidentialCommitment)
}

func TestAssessPropagatesFeatureViolations(t *testing.T) {
	t.Parallel()

	in := referenceAssessInput()
	in.Features.LatePaymentRate = money.MustParseRate("1.5")

	_, err := risk.ModelV1().Assess(in, testNow)
	require.ErrorIs(t, err, apperr.ErrValidation)
	assert.Contains(t, fieldNames(apperr.Fields(err)), "features.late_payment_rate")
}

// TestRankedContributionsExplainTheScore checks the ordering an AI explanation narrates.
// The numbers come from the model; the AI only describes them.
func TestRankedContributionsExplainTheScore(t *testing.T) {
	t.Parallel()

	got, err := risk.ModelV1().Assess(referenceAssessInput(), testNow)
	require.NoError(t, err)

	ranked := got.RankedContributions()
	require.Len(t, ranked, len(risk.FeatureNames()))

	assert.Equal(t, []string{
		"dso_norm",
		"debtor_risk",
		"late_payment_rate",
		"debtor_concentration",
		"market_volatility",
		"dispute_flag",
	}, contributionNames(ranked))

	assert.Equal(t, "0.720000", ranked[0].Effect.StringFixed(6), "1.8 * 0.40")
	assert.Equal(t, "0.000000", ranked[len(ranked)-1].Effect.StringFixed(6), "an absent dispute clause adds nothing")

	assert.Equal(t, contributionNames(ranked), contributionNames(got.RankedContributions()), "ordering is stable")
	assert.Equal(t, risk.FeatureNames(), contributionNames(got.Contributions), "the stored order is canonical")
}

func contributionNames(contributions []risk.Contribution) []string {
	names := make([]string, 0, len(contributions))
	for _, c := range contributions {
		names = append(names, c.Feature)
	}
	return names
}
