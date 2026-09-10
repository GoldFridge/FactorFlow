package risk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/GoldFridge/factorflow/internal/platform/apperr"
	"github.com/GoldFridge/factorflow/internal/platform/money"
	"github.com/GoldFridge/factorflow/internal/platform/postgres"
)

// Repository stores risk assessments.
//
// There is no update method, by design: an assessment is immutable, and a re-run inserts a
// new row. That is what lets a judge recompute a published price from the exact inputs it
// was produced with, however many times the invoice was later reassessed.
type Repository interface {
	Save(ctx context.Context, q postgres.Querier, assessment *Assessment) error
	Get(ctx context.Context, q postgres.Querier, id uuid.UUID) (*Assessment, error)
	Latest(ctx context.Context, q postgres.Querier, invoiceID uuid.UUID) (*Assessment, error)

	// SaveExplanation stores or replaces the words about an assessment. It is separate from
	// Save because the assessment is immutable and its narration is not.
	SaveExplanation(ctx context.Context, q postgres.Querier, e *Explanation) error
	GetExplanation(ctx context.Context, q postgres.Querier, assessmentID uuid.UUID) (*Explanation, error)
}

// PostgresRepository is the PostgreSQL implementation of Repository.
type PostgresRepository struct{}

// NewPostgresRepository returns the repository.
func NewPostgresRepository() *PostgresRepository { return &PostgresRepository{} }

const assessmentColumns = `
	id, invoice_id, model_version, features, contributions, confidence, pd, lgd,
	expected_loss_minor, grade, benchmark_apr, risk_premium, liquidity_premium,
	concentration_premium, discount_apr, platform_fee_minor, reserve_price_minor, currency,
	market_snapshot_hash, confidential_commitment, confidential_nonce, requires_manual_review, created_at`

// featureJSON is the stored shape of a feature vector.
type featureJSON map[string]string

// contributionJSON is the stored shape of one contribution.
type contributionJSON struct {
	Feature string `json:"feature"`
	Value   string `json:"value"`
	Weight  string `json:"weight"`
	Effect  string `json:"effect"`
}

// Save stores an assessment.
func (r *PostgresRepository) Save(ctx context.Context, q postgres.Querier, a *Assessment) error {
	features := featureJSON{}
	for _, ref := range featureRefs {
		features[ref.name] = ref.value(a.Features).StringFixed(rateScale)
	}
	encodedFeatures, err := json.Marshal(features)
	if err != nil {
		return err
	}

	contributions := make([]contributionJSON, 0, len(a.Contributions))
	for _, c := range a.Contributions {
		contributions = append(contributions, contributionJSON{
			Feature: c.Feature,
			Value:   c.Value.StringFixed(rateScale),
			Weight:  c.Weight.StringFixed(rateScale),
			Effect:  c.Effect.StringFixed(rateScale),
		})
	}
	encodedContributions, err := json.Marshal(contributions)
	if err != nil {
		return err
	}

	const query = `
		INSERT INTO risk_assessments (` + assessmentColumns + `)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23)`

	_, err = q.Exec(ctx, query,
		a.ID, a.InvoiceID, a.ModelVersion, encodedFeatures, encodedContributions,
		a.Confidence.Decimal(), a.PD.Decimal(), a.LGD.Decimal(), a.ExpectedLoss.Minor(), a.Grade.String(),
		a.BenchmarkAPR.Decimal(), a.Premiums.Risk.Decimal(), a.Premiums.Liquidity.Decimal(),
		a.Premiums.Concentration.Decimal(), a.DiscountAPR.Decimal(),
		a.PlatformFee.Minor(), a.ReservePrice.Minor(), a.ReservePrice.Currency().String(),
		a.MarketSnapshotHash, a.ConfidentialCommitment, a.ConfidentialNonce, a.RequiresManualReview, a.CreatedAt)
	return postgres.Translate(err)
}

// Get returns one assessment.
func (r *PostgresRepository) Get(ctx context.Context, q postgres.Querier, id uuid.UUID) (*Assessment, error) {
	const query = `SELECT ` + assessmentColumns + ` FROM risk_assessments WHERE id = $1`

	assessment, err := scanAssessment(q.QueryRow(ctx, query, id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, apperr.NotFoundf("risk assessment %s", id)
		}
		return nil, postgres.Translate(err)
	}
	return assessment, nil
}

// Latest returns an invoice's most recent assessment.
func (r *PostgresRepository) Latest(ctx context.Context, q postgres.Querier, invoiceID uuid.UUID) (*Assessment, error) {
	const query = `
		SELECT ` + assessmentColumns + `
		  FROM risk_assessments
		 WHERE invoice_id = $1
		 ORDER BY created_at DESC, id
		 LIMIT 1`

	assessment, err := scanAssessment(q.QueryRow(ctx, query, invoiceID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, apperr.NotFoundf("risk assessment for invoice %s", invoiceID)
		}
		return nil, postgres.Translate(err)
	}
	return assessment, nil
}

// row is the shared shape of pgx.Row and pgx.Rows for scanning.
type row interface {
	Scan(dest ...any) error
}

func scanAssessment(r row) (*Assessment, error) {
	var (
		a                   Assessment
		featuresRaw         []byte
		contributionsRaw    []byte
		confidence          decimal.Decimal
		pd                  decimal.Decimal
		lgd                 decimal.Decimal
		expectedLossMinor   int64
		grade               string
		benchmark           decimal.Decimal
		riskPremium         decimal.Decimal
		liquidityPremium    decimal.Decimal
		concentrationPremum decimal.Decimal
		discount            decimal.Decimal
		feeMinor            int64
		reserveMinor        int64
		currency            string
		createdAt           time.Time
	)

	if err := r.Scan(&a.ID, &a.InvoiceID, &a.ModelVersion, &featuresRaw, &contributionsRaw,
		&confidence, &pd, &lgd, &expectedLossMinor, &grade, &benchmark, &riskPremium,
		&liquidityPremium, &concentrationPremum, &discount, &feeMinor, &reserveMinor, &currency,
		&a.MarketSnapshotHash, &a.ConfidentialCommitment, &a.ConfidentialNonce, &a.RequiresManualReview, &createdAt); err != nil {
		return nil, err
	}

	parsedCurrency, err := money.ParseCurrency(currency)
	if err != nil {
		return nil, err
	}
	expectedLoss, err := money.New(expectedLossMinor, parsedCurrency)
	if err != nil {
		return nil, err
	}
	fee, err := money.New(feeMinor, parsedCurrency)
	if err != nil {
		return nil, err
	}
	reserve, err := money.New(reserveMinor, parsedCurrency)
	if err != nil {
		return nil, err
	}
	parsedGrade, err := ParseGrade(grade)
	if err != nil {
		return nil, err
	}

	var features featureJSON
	if err := json.Unmarshal(featuresRaw, &features); err != nil {
		return nil, err
	}
	for _, ref := range featureRefs {
		rate, err := money.ParseRate(features[ref.name])
		if err != nil {
			return nil, apperr.Invalid("features."+ref.name, "stored value %q is not a decimal", features[ref.name])
		}
		ref.set(&a.Features, rate)
	}

	var contributions []contributionJSON
	if err := json.Unmarshal(contributionsRaw, &contributions); err != nil {
		return nil, err
	}
	for _, stored := range contributions {
		value, err := money.ParseRate(stored.Value)
		if err != nil {
			return nil, err
		}
		weight, err := money.ParseRate(stored.Weight)
		if err != nil {
			return nil, err
		}
		effect, err := money.ParseRate(stored.Effect)
		if err != nil {
			return nil, err
		}
		a.Contributions = append(a.Contributions, Contribution{
			Feature: stored.Feature, Value: value, Weight: weight, Effect: effect,
		})
	}

	a.Confidence = money.NewRate(confidence)
	a.PD = money.NewRate(pd)
	a.LGD = money.NewRate(lgd)
	a.ExpectedLoss = expectedLoss
	a.Grade = parsedGrade
	a.BenchmarkAPR = money.NewRate(benchmark)
	a.Premiums = Premiums{
		Risk:          money.NewRate(riskPremium),
		Liquidity:     money.NewRate(liquidityPremium),
		Concentration: money.NewRate(concentrationPremum),
	}
	a.DiscountAPR = money.NewRate(discount)
	a.PlatformFee = fee
	a.ReservePrice = reserve
	a.CreatedAt = createdAt.UTC()
	return &a, nil
}

// SaveExplanation stores the narration of an assessment, replacing whatever was there.
//
// Replacement is the normal case rather than an edge one: the derived explanation is
// written with the assessment so a reader is never left without words, and a model's is
// written over it if one answers and what it wrote survives checking.
func (r *PostgresRepository) SaveExplanation(ctx context.Context, q postgres.Querier, e *Explanation) error {
	bullets, err := json.Marshal(e.Bullets)
	if err != nil {
		return fmt.Errorf("encoding the explanation: %w", err)
	}

	const query = `
		INSERT INTO assessment_explanations (assessment_id, source, model, bullets, created_at)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (assessment_id) DO UPDATE
		   SET source = EXCLUDED.source,
		       model = EXCLUDED.model,
		       bullets = EXCLUDED.bullets,
		       created_at = EXCLUDED.created_at`

	_, err = q.Exec(ctx, query, e.AssessmentID, e.Source, e.Model, bullets, e.CreatedAt)
	return postgres.Translate(err)
}

// GetExplanation returns the words stored about an assessment.
func (r *PostgresRepository) GetExplanation(ctx context.Context, q postgres.Querier, assessmentID uuid.UUID) (*Explanation, error) {
	const query = `
		SELECT assessment_id, source, model, bullets, created_at
		  FROM assessment_explanations
		 WHERE assessment_id = $1`

	var (
		e       Explanation
		bullets []byte
	)
	if err := q.QueryRow(ctx, query, assessmentID).
		Scan(&e.AssessmentID, &e.Source, &e.Model, &bullets, &e.CreatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, apperr.NotFoundf("explanation of assessment %s", assessmentID)
		}
		return nil, postgres.Translate(err)
	}
	if err := json.Unmarshal(bullets, &e.Bullets); err != nil {
		return nil, fmt.Errorf("decoding the explanation: %w", err)
	}

	e.CreatedAt = e.CreatedAt.UTC()
	return &e, nil
}
