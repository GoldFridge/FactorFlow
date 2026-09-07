package risk

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/GoldFridge/factorflow/internal/platform/apperr"
	"github.com/GoldFridge/factorflow/internal/platform/money"
)

// WorkflowRequest is what the confidential workflow needs to assess one invoice.
//
// It names the ciphertext and the wrapped key, never the document and never the key
// itself: the plaintext exists only inside the browser before upload and inside the TEE
// during processing.
type WorkflowRequest struct {
	InvoiceID  uuid.UUID
	ObjectKey  string
	CipherHash string
	KeyRef     string
	MIME       string
	// Nonce binds one workflow run to one request, so a replayed result is detectable.
	Nonce string
	// SchemaVersion is the feature schema the caller expects back.
	SchemaVersion string
}

// WorkflowResult is the minimal output a confidential run is allowed to return.
//
// There is no field for document text, line items or a raw model response, because none of
// those may leave the enclave. What comes back is a whitelisted feature vector, the
// validation verdicts, and a commitment that binds this result to that input.
type WorkflowResult struct {
	InvoiceID uuid.UUID
	// Features are the normalized risk features, already clamped to [0,1] inside the TEE.
	Features FeatureVector
	// Confidence is the extraction confidence the gate is applied to.
	Confidence money.Rate
	// ArithmeticValid reports whether the document's own totals and dates checked out.
	ArithmeticValid bool
	// Mitigations are the credit enhancements found in the document.
	Mitigations Mitigations
	// Commitment binds the result to its input, the model version and the nonce.
	Commitment string
	// SchemaVersion and ModelVersion identify what produced the result.
	SchemaVersion string
	ModelVersion  string
	// Evidence points at the run's simulation or deployment log, which is what a judge
	// checks rather than taking the result on trust.
	Evidence string
}

// Validate checks a result before any of it is used.
//
// A workflow is an external system, so its output is treated as untrusted input: a result
// for the wrong invoice, with features outside their range or without a commitment, is
// rejected rather than scored.
func (r WorkflowResult) Validate(request WorkflowRequest) error {
	if r.InvoiceID != request.InvoiceID {
		return apperr.Invalid("invoice_id", "the workflow answered for invoice %s, not %s", r.InvoiceID, request.InvoiceID)
	}
	if request.SchemaVersion != "" && r.SchemaVersion != request.SchemaVersion {
		return apperr.Invalid("schema_version", "expected %s, got %s", request.SchemaVersion, r.SchemaVersion)
	}
	if !commitmentHex.MatchString(strings.ToLower(r.Commitment)) {
		return apperr.Invalid("commitment", "must be a 0x-prefixed SHA-256 digest")
	}
	if r.Confidence.IsNegative() || r.Confidence.Cmp(money.OneRate()) > 0 {
		return apperr.Invalid("confidence", "must be in [0,1], got %s", r.Confidence)
	}
	return r.Features.Validate()
}

// Workflow runs the confidential assessment.
//
// The live implementation is a Chainlink CRE workflow whose Go handler decrypts the
// document inside a TEE. Everything behind this interface is replaceable; what the rest of
// the system depends on is the shape of the result, not where it ran.
type Workflow interface {
	Assess(ctx context.Context, request WorkflowRequest) (WorkflowResult, error)
}

// DeterministicWorkflow derives a feature vector from the request itself.
//
// It backs local development and the offline demo. It is honest about what it is: it does
// not read the document at all, it derives stable pseudo-features from the ciphertext
// hash, so the same invoice always scores the same and a demo can be rehearsed without a
// TEE. It is selected only when no CRE endpoint is configured.
type DeterministicWorkflow struct {
	// SchemaVersion is echoed back so the caller's schema check behaves as it will in
	// production.
	SchemaVersion string
}

// NewDeterministicWorkflow returns the offline workflow.
func NewDeterministicWorkflow() *DeterministicWorkflow {
	return &DeterministicWorkflow{SchemaVersion: FeatureSchemaV1}
}

// FeatureSchemaV1 is the feature schema this package understands.
const FeatureSchemaV1 = "features-v1"

// Assess derives features deterministically from the request.
func (w *DeterministicWorkflow) Assess(_ context.Context, request WorkflowRequest) (WorkflowResult, error) {
	if request.InvoiceID == uuid.Nil {
		return WorkflowResult{}, apperr.Invalid("invoice_id", "must be a non-nil UUID")
	}
	if request.CipherHash == "" {
		return WorkflowResult{}, apperr.Invalid("cipher_hash", "must not be empty")
	}

	seed := sha256.Sum256([]byte(request.CipherHash + "|" + request.InvoiceID.String()))
	feature := func(index int) money.Rate {
		// Two bytes give 1/65535 granularity, which is finer than the six digits the model
		// keeps, so the derived value survives quantization unchanged.
		raw := int64(seed[index])<<8 | int64(seed[index+1])
		rate, err := money.RateFromFraction(raw, 65535)
		if err != nil {
			return money.ZeroRate()
		}
		return rate.Quantize(rateScale)
	}

	schema := w.SchemaVersion
	if schema == "" {
		schema = FeatureSchemaV1
	}

	features := FeatureVector{
		DSONorm:             feature(0),
		LatePaymentRate:     feature(2),
		DisputeFlag:         money.ZeroRate(),
		DebtorConcentration: feature(4),
		MarketVolatility:    feature(6),
		DebtorRisk:          feature(8),
	}
	// A dispute clause is a flag, not a gradient: it is present in a fixed minority of the
	// demo set rather than at a random strength.
	if seed[10]%4 == 0 {
		features.DisputeFlag = money.OneRate()
	}

	confidence, err := money.RateFromFraction(int64(880+int(seed[11])%120), 1000)
	if err != nil {
		return WorkflowResult{}, err
	}

	result := WorkflowResult{
		InvoiceID:       request.InvoiceID,
		Features:        features,
		Confidence:      confidence.Quantize(rateScale),
		ArithmeticValid: true,
		Mitigations:     Mitigations{Recourse: seed[12]%2 == 0, Collateralized: seed[13]%3 == 0},
		SchemaVersion:   schema,
		ModelVersion:    ModelVersionV1,
		Evidence:        "deterministic-local-workflow",
	}
	result.Commitment = Commit(request, result)
	return result, nil
}

// Commit binds a workflow result to its request.
//
// The commitment is what a later verifier checks: it proves the stored features belong to
// this ciphertext and this nonce, without the commitment itself revealing anything about
// the document.
func Commit(request WorkflowRequest, result WorkflowResult) string {
	var b strings.Builder

	b.WriteString(request.InvoiceID.String())
	b.WriteString("|")
	b.WriteString(strings.ToLower(request.CipherHash))
	b.WriteString("|")
	b.WriteString(request.Nonce)
	b.WriteString("|")
	b.WriteString(result.SchemaVersion)
	b.WriteString("|")
	b.WriteString(result.ModelVersion)

	for _, contribution := range result.Features.contributions(ModelV1().Coefficients) {
		b.WriteString("|")
		b.WriteString(contribution.Feature)
		b.WriteString("=")
		b.WriteString(contribution.Value.StringFixed(rateScale))
	}

	b.WriteString("|confidence=")
	b.WriteString(result.Confidence.StringFixed(rateScale))
	b.WriteString("|arithmetic=")
	b.WriteString(strconv.FormatBool(result.ArithmeticValid))
	b.WriteString("|recourse=")
	b.WriteString(strconv.FormatBool(result.Mitigations.Recourse))
	b.WriteString("|collateral=")
	b.WriteString(strconv.FormatBool(result.Mitigations.Collateralized))

	sum := sha256.Sum256([]byte(b.String()))
	return "0x" + hex.EncodeToString(sum[:])
}
