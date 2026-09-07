package assessment_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoldFridge/factorflow/internal/app/assessment"
	"github.com/GoldFridge/factorflow/internal/invoice"
	"github.com/GoldFridge/factorflow/internal/marketdata"
	"github.com/GoldFridge/factorflow/internal/platform/apperr"
	"github.com/GoldFridge/factorflow/internal/platform/money"
	"github.com/GoldFridge/factorflow/internal/platform/outbox"
	"github.com/GoldFridge/factorflow/internal/platform/postgres"
	"github.com/GoldFridge/factorflow/internal/risk"
)

var testNow = time.Date(2026, time.September, 5, 12, 0, 0, 0, time.UTC)

// The fakes below stand in for storage so the worker's own logic is what a failure points
// at: which collaborator it calls, what it does when one fails, and what it writes together.

type fakeQuerier struct{}

func (fakeQuerier) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.NewCommandTag("INSERT 0 1"), nil
}
func (fakeQuerier) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("not used")
}
func (fakeQuerier) QueryRow(context.Context, string, ...any) pgx.Row { panic("not used") }

// fakeDB gives the fakes transaction semantics: writes made inside a unit of work are
// discarded when it fails, so a test can check that a failed commit leaves nothing behind.
type fakeDB struct {
	commitErr   error
	invoices    *fakeInvoices
	assessments *fakeAssessments
	snapshots   *fakeSnapshots
}

func (f *fakeDB) InTx(_ context.Context, fn func(postgres.Querier) error) error {
	invoicesBefore := make(map[uuid.UUID]invoice.Invoice, len(f.invoices.stored))
	for k, v := range f.invoices.stored {
		invoicesBefore[k] = v
	}
	assessmentsBefore := append([]*risk.Assessment(nil), f.assessments.saved...)
	snapshotsBefore := append([]*marketdata.Snapshot(nil), f.snapshots.saved...)

	err := fn(fakeQuerier{})
	if err == nil {
		err = f.commitErr
	}
	if err != nil {
		f.invoices.stored = invoicesBefore
		f.assessments.saved = assessmentsBefore
		f.snapshots.saved = snapshotsBefore
		return err
	}
	return nil
}

func (f *fakeDB) Querier() postgres.Querier { return fakeQuerier{} }

type fakeInvoices struct {
	stored     map[uuid.UUID]invoice.Invoice
	updateFail error
}

func (f *fakeInvoices) Create(context.Context, postgres.Querier, *invoice.Invoice) error { return nil }

func (f *fakeInvoices) Get(_ context.Context, _ postgres.Querier, id uuid.UUID) (*invoice.Invoice, error) {
	stored, ok := f.stored[id]
	if !ok {
		return nil, apperr.NotFoundf("invoice %s", id)
	}
	copied := stored
	return &copied, nil
}

func (f *fakeInvoices) Update(_ context.Context, _ postgres.Querier, inv *invoice.Invoice, expected int64) error {
	if f.updateFail != nil {
		return f.updateFail
	}
	stored, ok := f.stored[inv.ID]
	if !ok {
		return apperr.NotFoundf("invoice %s", inv.ID)
	}
	if stored.Version != expected {
		return apperr.Conflictf("invoice %s was modified by another writer", inv.ID)
	}
	f.stored[inv.ID] = *inv
	return nil
}

func (f *fakeInvoices) ListByIssuer(context.Context, postgres.Querier, uuid.UUID, int) ([]*invoice.Invoice, error) {
	return nil, nil
}
func (f *fakeInvoices) ListByStatus(context.Context, postgres.Querier, invoice.Status, int) ([]*invoice.Invoice, error) {
	return nil, nil
}
func (f *fakeInvoices) SaveDocument(context.Context, postgres.Querier, *invoice.Document) error {
	return nil
}
func (f *fakeInvoices) GetDocument(context.Context, postgres.Querier, uuid.UUID) (*invoice.Document, error) {
	return nil, apperr.NotFoundf("document")
}

type fakeAssessments struct {
	saved    []*risk.Assessment
	saveFail error
}

func (f *fakeAssessments) Save(_ context.Context, _ postgres.Querier, a *risk.Assessment) error {
	if f.saveFail != nil {
		return f.saveFail
	}
	f.saved = append(f.saved, a)
	return nil
}
func (f *fakeAssessments) Get(context.Context, postgres.Querier, uuid.UUID) (*risk.Assessment, error) {
	return nil, apperr.NotFoundf("assessment")
}
func (f *fakeAssessments) Latest(context.Context, postgres.Querier, uuid.UUID) (*risk.Assessment, error) {
	if len(f.saved) == 0 {
		return nil, apperr.NotFoundf("assessment")
	}
	return f.saved[len(f.saved)-1], nil
}

type fakeSnapshots struct{ saved []*marketdata.Snapshot }

func (f *fakeSnapshots) Save(_ context.Context, _ postgres.Querier, s *marketdata.Snapshot) error {
	f.saved = append(f.saved, s)
	return nil
}
func (f *fakeSnapshots) Get(context.Context, postgres.Querier, string) (*marketdata.Snapshot, error) {
	return nil, apperr.NotFoundf("snapshot")
}
func (f *fakeSnapshots) Latest(context.Context, postgres.Querier, string, string) (*marketdata.Snapshot, error) {
	if len(f.saved) == 0 {
		return nil, apperr.NotFoundf("snapshot")
	}
	return f.saved[len(f.saved)-1], nil
}

// failingWorkflow stands in for a TEE that is not answering.
type failingWorkflow struct{ err error }

func (f failingWorkflow) Assess(context.Context, risk.WorkflowRequest) (risk.WorkflowResult, error) {
	return risk.WorkflowResult{}, f.err
}

// lyingWorkflow returns a result that does not belong to the request.
type lyingWorkflow struct{ mutate func(*risk.WorkflowResult) }

func (l lyingWorkflow) Assess(ctx context.Context, request risk.WorkflowRequest) (risk.WorkflowResult, error) {
	result, err := risk.NewDeterministicWorkflow().Assess(ctx, request)
	if err != nil {
		return result, err
	}
	l.mutate(&result)
	return result, nil
}

// workerFixture wires a worker over the fakes.
type workerFixture struct {
	worker      *assessment.AssessmentWorker
	invoices    *fakeInvoices
	assessments *fakeAssessments
	snapshots   *fakeSnapshots
	db          *fakeDB
	invoice     *invoice.Invoice
	clock       time.Time
}

func newWorkerFixture(t *testing.T, workflow risk.Workflow) *workerFixture {
	t.Helper()

	inv, err := invoice.New(invoice.NewParams{
		ID:        uuid.MustParse("33333333-3333-4333-8333-333333333333"),
		IssuerID:  uuid.New(),
		DebtorRef: "ACME Logistics GmbH",
		Number:    "INV-2026-0042",
		Face:      money.MustParse("10000.00", money.USD),
		IssuedAt:  testNow,
		DueAt:     testNow.Add(60 * 24 * time.Hour),
	}, testNow)
	require.NoError(t, err)
	require.NoError(t, inv.MarkUploaded(testNow))
	require.NoError(t, inv.StartAssessment(testNow))

	f := &workerFixture{
		invoices:    &fakeInvoices{stored: map[uuid.UUID]invoice.Invoice{inv.ID: *inv}},
		assessments: &fakeAssessments{},
		snapshots:   &fakeSnapshots{},
		invoice:     inv,
		clock:       testNow,
	}
	f.db = &fakeDB{invoices: f.invoices, assessments: f.assessments, snapshots: f.snapshots}

	if workflow == nil {
		workflow = risk.NewDeterministicWorkflow()
	}

	market := marketdata.NewService(
		marketdata.NewStaticProvider(marketdata.DemoMarkets()...),
		marketdata.NewNormalizer(),
		func() time.Time { return f.clock },
	)

	f.worker = assessment.NewAssessmentWorker(assessment.WorkerConfig{
		DB:          f.db,
		Invoices:    f.invoices,
		Assessments: f.assessments,
		Snapshots:   f.snapshots,
		Market:      market,
		Workflow:    workflow,
		Model:       risk.ModelV1(),
		Query:       marketdata.DemoQuery(),
		Now:         func() time.Time { return f.clock },
		IDs:         uuid.New,
	})
	return f
}

func (f *workerFixture) event(t *testing.T) outbox.Event {
	t.Helper()

	payload, err := json.Marshal(map[string]any{
		"invoice_id":  f.invoice.ID,
		"object_key":  "invoices/2026/09/demo.enc",
		"cipher_hash": "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08",
		"key_ref":     "cre-secret://data-key/01J8Z9",
		"mime":        "application/pdf",
	})
	require.NoError(t, err)
	return outbox.Event{ID: 1, Topic: invoice.TopicAssess, Payload: payload}
}

// TestWorkerAssessesEndToEnd is the deterministic core in one path: a confidential result,
// a live market snapshot, and a priced, stored assessment.
func TestWorkerAssessesEndToEnd(t *testing.T) {
	t.Parallel()

	f := newWorkerFixture(t, nil)
	require.NoError(t, f.worker.Handle(context.Background(), f.event(t)))

	require.Len(t, f.assessments.saved, 1)
	assessment := f.assessments.saved[0]

	assert.Equal(t, f.invoice.ID, assessment.InvoiceID)
	assert.Equal(t, risk.ModelVersionV1, assessment.ModelVersion)
	assert.True(t, assessment.ReservePrice.IsPositive())
	assert.True(t, assessment.PD.IsPositive())
	assert.True(t, assessment.Grade.IsValid())
	assert.Regexp(t, `^0x[0-9a-f]{64}$`, assessment.ConfidentialCommitment)

	require.Len(t, f.snapshots.saved, 1)
	assert.Equal(t, f.snapshots.saved[0].PayloadHash, assessment.MarketSnapshotHash,
		"the price points at the snapshot it was computed from")
	assert.True(t, assessment.Features.MarketVolatility.Equal(f.snapshots.saved[0].Volatility),
		"volatility comes from the market, not from the document")

	stored := f.invoices.stored[f.invoice.ID]
	assert.Equal(t, invoice.StatusAssessed, stored.Status)
	assert.Equal(t, assessment.ID, stored.AssessmentID)
}

// TestWorkerIsIdempotent covers at-least-once delivery: a redelivered event must not score
// the same invoice twice.
func TestWorkerIsIdempotent(t *testing.T) {
	t.Parallel()

	f := newWorkerFixture(t, nil)
	event := f.event(t)

	require.NoError(t, f.worker.Handle(context.Background(), event))
	require.NoError(t, f.worker.Handle(context.Background(), event))

	assert.Len(t, f.assessments.saved, 1, "the second delivery did nothing")
}

func TestWorkerIsReproducible(t *testing.T) {
	t.Parallel()

	first := newWorkerFixture(t, nil)
	require.NoError(t, first.worker.Handle(context.Background(), first.event(t)))

	second := newWorkerFixture(t, nil)
	require.NoError(t, second.worker.Handle(context.Background(), second.event(t)))

	a, b := first.assessments.saved[0], second.assessments.saved[0]
	assert.Equal(t, a.PD.String(), b.PD.String())
	assert.Equal(t, a.LGD.String(), b.LGD.String())
	assert.Equal(t, a.DiscountAPR.String(), b.DiscountAPR.String())
	assert.Equal(t, a.ReservePrice.String(), b.ReservePrice.String())
	assert.Equal(t, a.MarketSnapshotHash, b.MarketSnapshotHash)

	// The commitment is deliberately not reproducible across runs: it binds one workflow
	// run through its nonce, which is what makes a replayed result detectable.
	assert.NotEqual(t, a.ConfidentialCommitment, b.ConfidentialCommitment)
	assert.NotEqual(t, a.ConfidentialNonce, b.ConfidentialNonce)
}

// TestStoredCommitmentIsCheckable is why the nonce is stored: a commitment nobody can
// recompute proves nothing.
func TestStoredCommitmentIsCheckable(t *testing.T) {
	t.Parallel()

	f := newWorkerFixture(t, nil)
	require.NoError(t, f.worker.Handle(context.Background(), f.event(t)))

	stored := f.assessments.saved[0]
	require.NotEmpty(t, stored.ConfidentialNonce)

	// Recompute the commitment from what was stored, exactly as a verifier would.
	recomputed := risk.Commit(
		risk.WorkflowRequest{
			InvoiceID:  stored.InvoiceID,
			CipherHash: "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08",
			Nonce:      stored.ConfidentialNonce,
		},
		risk.WorkflowResult{
			Features:        workflowFeaturesOf(t, stored),
			Confidence:      stored.Confidence,
			ArithmeticValid: true,
			Mitigations:     mitigationsOf(stored),
			SchemaVersion:   risk.FeatureSchemaV1,
			ModelVersion:    risk.ModelVersionV1,
		})

	assert.Equal(t, stored.ConfidentialCommitment, recomputed)
}

// workflowFeaturesOf rebuilds the vector the workflow returned. The stored vector carries
// the market's volatility rather than the workflow's, so that one field is taken from the
// workflow result the fixture can reproduce.
func workflowFeaturesOf(t *testing.T, stored *risk.Assessment) risk.FeatureVector {
	t.Helper()

	original, err := risk.NewDeterministicWorkflow().Assess(context.Background(), risk.WorkflowRequest{
		InvoiceID:  stored.InvoiceID,
		CipherHash: "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08",
	})
	require.NoError(t, err)
	return original.Features
}

func mitigationsOf(stored *risk.Assessment) risk.Mitigations {
	model := risk.ModelV1()
	for _, candidate := range []risk.Mitigations{
		{},
		{Recourse: true},
		{Collateralized: true},
		{Recourse: true, Collateralized: true},
	} {
		if model.LossGivenDefault(candidate).Equal(stored.LGD) {
			return candidate
		}
	}
	return risk.Mitigations{}
}

// TestWorkflowFailureFailsTheInvoiceAndRetries checks both halves of the failure path: the
// invoice records which stage failed, and the error propagates so the outbox retries.
func TestWorkflowFailureFailsTheInvoiceAndRetries(t *testing.T) {
	t.Parallel()

	f := newWorkerFixture(t, failingWorkflow{err: errors.New("TEE did not respond")})

	err := f.worker.Handle(context.Background(), f.event(t))
	require.Error(t, err)
	assert.ErrorIs(t, err, apperr.ErrUnavailable)

	stored := f.invoices.stored[f.invoice.ID]
	assert.Equal(t, invoice.StatusFailed, stored.Status)
	assert.Equal(t, invoice.StatusExtracting, stored.FailedFrom)
	assert.Contains(t, stored.Reason, "TEE did not respond")
	assert.Empty(t, f.assessments.saved)
}

// TestUntrustedWorkflowResultsAreRejected treats the workflow as what it is: an external
// system whose output is input, not truth.
func TestUntrustedWorkflowResultsAreRejected(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*risk.WorkflowResult)
	}{
		{
			name:   "an answer for another invoice",
			mutate: func(r *risk.WorkflowResult) { r.InvoiceID = uuid.New() },
		},
		{
			name:   "a feature outside its range",
			mutate: func(r *risk.WorkflowResult) { r.Features.DSONorm = money.MustParseRate("1.5") },
		},
		{
			name:   "confidence outside its range",
			mutate: func(r *risk.WorkflowResult) { r.Confidence = money.MustParseRate("1.2") },
		},
		{
			name:   "a missing commitment",
			mutate: func(r *risk.WorkflowResult) { r.Commitment = "" },
		},
		{
			name:   "another feature schema",
			mutate: func(r *risk.WorkflowResult) { r.SchemaVersion = "features-v2" },
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := newWorkerFixture(t, lyingWorkflow{mutate: tc.mutate})

			err := f.worker.Handle(context.Background(), f.event(t))
			require.ErrorIs(t, err, apperr.ErrValidation)

			assert.Empty(t, f.assessments.saved)
			assert.Equal(t, invoice.StatusFailed, f.invoices.stored[f.invoice.ID].Status)
		})
	}
}

// TestStaleMarketDataBlocksPricing is the DATA_STALE rule reaching the pricing path.
func TestStaleMarketDataBlocksPricing(t *testing.T) {
	t.Parallel()

	f := newWorkerFixture(t, nil)

	// The service observes at the fixture's clock; move the clock only for the freshness
	// check by making the observation older than the TTL.
	f.clock = testNow
	market := marketdata.NewService(
		marketdata.NewStaticProvider(marketdata.DemoMarkets()...),
		marketdata.NewNormalizer(),
		func() time.Time { return testNow.Add(-time.Hour) },
	)
	f.worker = assessment.NewAssessmentWorker(assessment.WorkerConfig{
		DB: f.db, Invoices: f.invoices, Assessments: f.assessments, Snapshots: f.snapshots,
		Market: market, Workflow: risk.NewDeterministicWorkflow(), Model: risk.ModelV1(),
		Query: marketdata.DemoQuery(), Now: func() time.Time { return f.clock }, IDs: uuid.New,
	})

	err := f.worker.Handle(context.Background(), f.event(t))
	require.ErrorIs(t, err, apperr.ErrUnavailable)
	assert.Contains(t, err.Error(), "DATA_STALE")
	assert.Empty(t, f.assessments.saved)
}

func TestOverdueInvoiceCannotBePriced(t *testing.T) {
	t.Parallel()

	f := newWorkerFixture(t, nil)
	f.clock = testNow.Add(90 * 24 * time.Hour)

	err := f.worker.Handle(context.Background(), f.event(t))
	require.ErrorIs(t, err, apperr.ErrValidation)
	assert.Equal(t, invoice.StatusFailed, f.invoices.stored[f.invoice.ID].Status)
}

// TestNothingIsStoredWhenTheCommitFails keeps the three writes together: a snapshot or an
// assessment without the invoice pointing at it would be a price nobody can trace.
func TestNothingIsStoredWhenTheCommitFails(t *testing.T) {
	t.Parallel()

	f := newWorkerFixture(t, nil)
	f.db.commitErr = errors.New("commit failed")

	require.Error(t, f.worker.Handle(context.Background(), f.event(t)))
	assert.Equal(t, invoice.StatusExtracting, f.invoices.stored[f.invoice.ID].Status,
		"the invoice never moved, so the outbox will retry the whole assessment")
}

func TestMalformedCommandIsReported(t *testing.T) {
	t.Parallel()

	f := newWorkerFixture(t, nil)

	err := f.worker.Handle(context.Background(), outbox.Event{Payload: []byte("{not json")})
	require.Error(t, err)

	err = f.worker.Handle(context.Background(), outbox.Event{Payload: []byte(`{"invoice_id":null}`)})
	require.ErrorIs(t, err, apperr.ErrValidation)
}

func TestUnknownInvoiceIsReported(t *testing.T) {
	t.Parallel()

	f := newWorkerFixture(t, nil)
	payload, err := json.Marshal(map[string]any{"invoice_id": uuid.New(), "cipher_hash": "abc"})
	require.NoError(t, err)

	err = f.worker.Handle(context.Background(), outbox.Event{Payload: payload})
	require.ErrorIs(t, err, apperr.ErrNotFound)
}

// TestAlreadyMovedInvoiceIsSkipped covers the redelivery that arrives after a human already
// rejected the invoice.
func TestAlreadyMovedInvoiceIsSkipped(t *testing.T) {
	t.Parallel()

	f := newWorkerFixture(t, nil)

	stored := f.invoices.stored[f.invoice.ID]
	require.NoError(t, stored.FailAssessment("operator cancelled", testNow))
	f.invoices.stored[f.invoice.ID] = stored

	require.NoError(t, f.worker.Handle(context.Background(), f.event(t)))
	assert.Empty(t, f.assessments.saved)
}
