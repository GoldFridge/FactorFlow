package risk

import (
	"errors"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/skimer2king/factorflow/internal/platform/apperr"
	"github.com/skimer2king/factorflow/internal/platform/money"
)

// commitmentHex matches the 0x-prefixed SHA-256 digests used for the market snapshot hash
// and the confidential output commitment.
var commitmentHex = regexp.MustCompile(`^0x[0-9a-f]{64}$`)

// Assessment is the immutable result of scoring one invoice.
//
// It records not just the numbers but everything needed to recompute them: the feature
// vector, the model version, the market snapshot the benchmark came from, and the
// commitment to the confidential workflow output. A stored assessment is never mutated; a
// re-run produces a new version.
type Assessment struct {
	ID           uuid.UUID
	InvoiceID    uuid.UUID
	ModelVersion string

	Features      FeatureVector
	Contributions []Contribution
	Confidence    money.Rate

	PD           money.Rate
	LGD          money.Rate
	ExpectedLoss money.Amount
	Grade        Grade

	BenchmarkAPR money.Rate
	Premiums     Premiums
	DiscountAPR  money.Rate
	PlatformFee  money.Amount
	ReservePrice money.Amount

	// MarketSnapshotHash binds the price to the exact market data it was computed from.
	MarketSnapshotHash string
	// ConfidentialCommitment binds the assessment to the signed minimal output of the TEE
	// workflow, without carrying any document content.
	ConfidentialCommitment string
	// ConfidentialNonce is the per-run nonce the commitment was computed over. It is stored
	// because a commitment nobody can recompute proves nothing: with the nonce, the stored
	// features and the model version, a verifier can check the commitment itself.
	ConfidentialNonce string

	// RequiresManualReview is set when the confidence gate blocks auto-approval.
	RequiresManualReview bool

	CreatedAt time.Time
}

// AssessInput is the whole input of an assessment: the invoice facts, the confidential
// workflow's minimal output, and the market snapshot's pricing inputs.
type AssessInput struct {
	ID        uuid.UUID
	InvoiceID uuid.UUID

	Face      money.Amount
	DaysToDue int64

	Features        FeatureVector
	Confidence      money.Rate
	Mitigations     Mitigations
	ArithmeticValid bool

	Benchmark        money.Rate
	LiquidityPremium money.Rate

	MarketSnapshotHash     string
	ConfidentialCommitment string
	ConfidentialNonce      string
}

// Assess scores an invoice and prices it.
//
// The function is pure: given the same input and model version it returns the same
// assessment, which is what lets a judge recompute a published price from stored inputs.
func (m Model) Assess(in AssessInput, now time.Time) (*Assessment, error) {
	if err := in.validate(); err != nil {
		return nil, err
	}

	pd, err := m.ProbabilityOfDefault(in.Features)
	if err != nil {
		return nil, err
	}
	lgd := m.LossGivenDefault(in.Mitigations)

	expectedLoss, err := m.ExpectedLoss(pd, lgd, in.Face)
	if err != nil {
		return nil, err
	}

	price, err := m.Price(PriceInput{
		Face:                in.Face,
		DaysToDue:           in.DaysToDue,
		Benchmark:           in.Benchmark,
		LiquidityPremium:    in.LiquidityPremium,
		PD:                  pd,
		LGD:                 lgd,
		DebtorConcentration: in.Features.DebtorConcentration,
	})
	if err != nil {
		return nil, err
	}

	return &Assessment{
		ID:                     in.ID,
		InvoiceID:              in.InvoiceID,
		ModelVersion:           m.Version,
		Features:               in.Features,
		Contributions:          in.Features.contributions(m.Coefficients),
		Confidence:             in.Confidence.Quantize(rateScale),
		PD:                     pd,
		LGD:                    lgd,
		ExpectedLoss:           expectedLoss,
		Grade:                  GradeFromPD(pd),
		BenchmarkAPR:           in.Benchmark.Quantize(rateScale),
		Premiums:               price.Premiums,
		DiscountAPR:            price.DiscountAPR,
		PlatformFee:            price.PlatformFee,
		ReservePrice:           price.ReservePrice,
		MarketSnapshotHash:     strings.ToLower(in.MarketSnapshotHash),
		ConfidentialCommitment: strings.ToLower(in.ConfidentialCommitment),
		ConfidentialNonce:      in.ConfidentialNonce,
		RequiresManualReview:   m.RequiresManualReview(in.Confidence, in.ArithmeticValid),
		CreatedAt:              now.UTC(),
	}, nil
}

func (in AssessInput) validate() error {
	var violations []error

	if in.ID == uuid.Nil {
		violations = append(violations, apperr.Invalid("id", "must be a non-nil UUID"))
	}
	if in.InvoiceID == uuid.Nil {
		violations = append(violations, apperr.Invalid("invoice_id", "must be a non-nil UUID"))
	}
	if in.Confidence.IsNegative() || in.Confidence.Cmp(money.OneRate()) > 0 {
		violations = append(violations, apperr.Invalid("confidence", "must be in [0,1], got %s", in.Confidence))
	}
	if !commitmentHex.MatchString(strings.ToLower(in.MarketSnapshotHash)) {
		violations = append(violations, apperr.Invalid("market_snapshot_hash", "must be a 0x-prefixed SHA-256 digest"))
	}
	if !commitmentHex.MatchString(strings.ToLower(in.ConfidentialCommitment)) {
		violations = append(violations, apperr.Invalid("confidential_commitment", "must be a 0x-prefixed SHA-256 digest"))
	}

	return errors.Join(violations...)
}

// RankedContributions returns the feature contributions ordered by absolute effect,
// largest first, with ties broken by feature name.
//
// This is the input an AI explanation is allowed to narrate. The ordering is deterministic
// so two runs explain the same assessment in the same order.
func (a *Assessment) RankedContributions() []Contribution {
	ranked := make([]Contribution, len(a.Contributions))
	copy(ranked, a.Contributions)

	sort.SliceStable(ranked, func(i, j int) bool {
		left := ranked[i].Effect.Decimal().Abs()
		right := ranked[j].Effect.Decimal().Abs()
		if !left.Equal(right) {
			return left.GreaterThan(right)
		}
		return ranked[i].Feature < ranked[j].Feature
	})
	return ranked
}
