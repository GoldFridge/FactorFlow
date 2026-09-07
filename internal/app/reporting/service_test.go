package reporting_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoldFridge/factorflow/internal/app/reporting"
	"github.com/GoldFridge/factorflow/internal/invoice"
	"github.com/GoldFridge/factorflow/internal/marketdata"
	"github.com/GoldFridge/factorflow/internal/platform/apperr"
	"github.com/GoldFridge/factorflow/internal/platform/audit"
	"github.com/GoldFridge/factorflow/internal/platform/httpserver"
	"github.com/GoldFridge/factorflow/internal/platform/money"
	"github.com/GoldFridge/factorflow/internal/risk"
	"github.com/GoldFridge/factorflow/internal/testsupport/memrepo"
)

var testNow = time.Date(2026, time.September, 7, 9, 0, 0, 0, time.UTC)

type fixture struct {
	service *reporting.Service
	store   *memrepo.Store
	router  http.Handler

	issuer   reporting.Actor
	stranger reporting.Actor
	operator reporting.Actor

	clock time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()

	store := memrepo.New()
	f := &fixture{
		store:    store,
		issuer:   reporting.Actor{OrganizationID: uuid.MustParse("11111111-1111-4111-8111-111111111111")},
		stranger: reporting.Actor{OrganizationID: uuid.MustParse("22222222-2222-4222-8222-222222222222")},
		operator: reporting.Actor{OrganizationID: uuid.MustParse("33333333-3333-4333-8333-333333333333"), Operator: true},
		clock:    testNow,
	}
	f.service = reporting.NewService(reporting.Config{
		DB:          store,
		Invoices:    store.Invoices(),
		Assessments: store.Assessments(),
		Snapshots:   store.Snapshots(),
		Timeline:    store.Audit(),
		Market:      marketdata.DemoQuery(),
	})

	handler := reporting.NewHandler(f.service, func() time.Time { return f.clock })
	f.router = httpserver.NewRouter(httpserver.Dependencies{
		Routes: func(r chi.Router) {
			r.Use(func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					next.ServeHTTP(w, req.WithContext(httpserver.ContextWithActor(req.Context(), f.actor(req))))
				})
			})
			handler.Routes(r)
		},
	})
	return f
}

// actor lets a test choose who is calling with a header, standing in for the session
// middleware the real router runs.
func (f *fixture) actor(r *http.Request) httpserver.Actor {
	switch r.Header.Get("X-Test-Actor") {
	case "issuer":
		return httpserver.Actor{OrganizationID: f.issuer.OrganizationID, Eligible: true}
	case "stranger":
		return httpserver.Actor{OrganizationID: f.stranger.OrganizationID, Eligible: true}
	case "operator":
		return httpserver.Actor{OrganizationID: f.operator.OrganizationID, Eligible: true, Operator: true}
	default:
		return httpserver.Actor{}
	}
}

func (f *fixture) get(t *testing.T, path, actor string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(http.MethodGet, path, http.NoBody)
	if actor != "" {
		req.Header.Set("X-Test-Actor", actor)
	}
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	return rec
}

// assessedInvoice seeds an invoice, the snapshot its benchmark came from, and the
// assessment that binds the two.
func (f *fixture) assessedInvoice(t *testing.T, issuerID uuid.UUID) (*invoice.Invoice, *risk.Assessment, *marketdata.Snapshot) {
	t.Helper()

	inv, err := invoice.New(invoice.NewParams{
		ID:        uuid.New(),
		IssuerID:  issuerID,
		DebtorRef: "ACME Logistics GmbH",
		Number:    "INV-2026-0001",
		Face:      money.MustParse("10000.00", money.USD),
		IssuedAt:  testNow,
		DueAt:     testNow.Add(60 * 24 * time.Hour),
	}, testNow)
	require.NoError(t, err)

	assessmentID := uuid.New()
	require.NoError(t, inv.MarkUploaded(testNow))
	require.NoError(t, inv.StartAssessment(testNow))
	require.NoError(t, inv.CompleteAssessment(assessmentID, testNow))
	f.store.PutInvoice(inv)

	snapshot := f.snapshot(t)
	require.NoError(t, f.store.Snapshots().Save(context.Background(), f.store.Querier(), snapshot))

	assessment, err := risk.ModelV1().Assess(risk.AssessInput{
		ID:        assessmentID,
		InvoiceID: inv.ID,
		Face:      inv.Face,
		DaysToDue: 60,
		Features: risk.FeatureVector{
			DSONorm:             money.MustParseRate("0.40"),
			LatePaymentRate:     money.MustParseRate("0.15"),
			DisputeFlag:         money.ZeroRate(),
			DebtorConcentration: money.MustParseRate("0.38"),
			MarketVolatility:    snapshot.Volatility,
			DebtorRisk:          money.MustParseRate("0.20"),
		},
		Confidence:             money.MustParseRate("0.93"),
		ArithmeticValid:        true,
		Benchmark:              snapshot.Benchmark,
		LiquidityPremium:       snapshot.LiquidityPremium,
		MarketSnapshotHash:     snapshot.PayloadHash,
		ConfidentialCommitment: "0x1a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5d6e7f809",
		ConfidentialNonce:      "0f1e2d3c4b5a69788796a5b4c3d2e1f0",
	}, testNow)
	require.NoError(t, err)
	f.store.PutAssessment(assessment)

	return inv, assessment, snapshot
}

func (f *fixture) snapshot(t *testing.T) *marketdata.Snapshot {
	t.Helper()

	service := marketdata.NewService(
		marketdata.NewStaticProvider(marketdata.DemoMarkets()...),
		marketdata.NewNormalizer(),
		func() time.Time { return testNow },
	)
	snapshot, err := service.Snapshot(context.Background(), marketdata.DemoQuery())
	require.NoError(t, err)
	return snapshot
}

// TestAssessmentExplainsThePrice is the point of the endpoint: the stored numbers come back
// with the market they were computed against and the feature effects behind the score.
func TestAssessmentExplainsThePrice(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	inv, assessment, snapshot := f.assessedInvoice(t, f.issuer.OrganizationID)

	report, err := f.service.AssessmentFor(t.Context(), f.issuer, inv.ID)
	require.NoError(t, err)

	assert.Equal(t, assessment.ID, report.Assessment.ID)
	assert.Equal(t, assessment.ReservePrice, report.Assessment.ReservePrice)
	require.NotNil(t, report.Snapshot, "the benchmark's own market is on record")
	assert.Equal(t, snapshot.PayloadHash, report.Snapshot.PayloadHash)
	assert.Equal(t, snapshot.Benchmark, report.Assessment.BenchmarkAPR,
		"the published benchmark is the snapshot's, not a re-derived one")

	require.Len(t, report.Contributions, len(risk.FeatureNames()))
	for i := 1; i < len(report.Contributions); i++ {
		previous := report.Contributions[i-1].Effect.Decimal().Abs()
		current := report.Contributions[i].Effect.Decimal().Abs()
		assert.False(t, current.GreaterThan(previous), "contributions come back largest first")
	}
}

// TestAssessmentSurvivesAPrunedSnapshot keeps an explanation readable when the market row
// behind it is gone: the score still has to be reportable.
func TestAssessmentSurvivesAPrunedSnapshot(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	inv, _, _ := f.assessedInvoice(t, f.issuer.OrganizationID)

	// A store with the invoice and assessment but no snapshot stands in for a pruned row.
	pruned := memrepo.New()
	stored, ok := f.store.Invoice(inv.ID)
	require.True(t, ok)
	pruned.PutInvoice(&stored)

	assessment, err := f.store.Assessments().Latest(t.Context(), f.store.Querier(), inv.ID)
	require.NoError(t, err)
	pruned.PutAssessment(assessment)

	service := reporting.NewService(reporting.Config{
		DB: pruned, Invoices: pruned.Invoices(), Assessments: pruned.Assessments(),
		Snapshots: pruned.Snapshots(), Market: marketdata.DemoQuery(),
	})

	report, err := service.AssessmentFor(t.Context(), f.issuer, inv.ID)
	require.NoError(t, err)
	assert.Nil(t, report.Snapshot)
	assert.Equal(t, assessment.ID, report.Assessment.ID)
}

// TestAssessmentIsHiddenFromStrangers checks that the refusal does not itself disclose that
// the invoice exists.
func TestAssessmentIsHiddenFromStrangers(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	inv, _, _ := f.assessedInvoice(t, f.issuer.OrganizationID)

	_, err := f.service.AssessmentFor(t.Context(), f.stranger, inv.ID)
	require.ErrorIs(t, err, apperr.ErrNotFound)

	_, err = f.service.AssessmentFor(t.Context(), f.operator, inv.ID)
	require.NoError(t, err, "an operator may read any assessment")
}

func TestAssessmentRequiresAnAssessedInvoice(t *testing.T) {
	t.Parallel()

	f := newFixture(t)

	inv, err := invoice.New(invoice.NewParams{
		ID:        uuid.New(),
		IssuerID:  f.issuer.OrganizationID,
		DebtorRef: "ACME Logistics GmbH",
		Number:    "INV-2026-0002",
		Face:      money.MustParse("5000.00", money.USD),
		IssuedAt:  testNow,
		DueAt:     testNow.Add(30 * 24 * time.Hour),
	}, testNow)
	require.NoError(t, err)
	f.store.PutInvoice(inv)

	_, err = f.service.AssessmentFor(t.Context(), f.issuer, inv.ID)
	require.ErrorIs(t, err, apperr.ErrNotFound)

	_, err = f.service.AssessmentFor(t.Context(), f.issuer, uuid.New())
	require.ErrorIs(t, err, apperr.ErrNotFound)
}

// TestLatestSnapshotReportsStaleness proves the endpoint describes rather than enforces:
// a participant may look at an expired benchmark and be told it is expired.
func TestLatestSnapshotReportsStaleness(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	snapshot := f.snapshot(t)
	require.NoError(t, f.store.Snapshots().Save(t.Context(), f.store.Querier(), snapshot))

	got, fresh, err := f.service.LatestSnapshot(t.Context(), f.issuer, testNow)
	require.NoError(t, err)
	assert.Equal(t, snapshot.PayloadHash, got.PayloadHash)
	assert.True(t, fresh)

	_, fresh, err = f.service.LatestSnapshot(t.Context(), f.issuer, snapshot.ExpiresAt().Add(time.Second))
	require.NoError(t, err)
	assert.False(t, fresh, "a stale benchmark is still visible, and still says it is stale")
}

func TestLatestSnapshotNeedsACaller(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	require.NoError(t, f.store.Snapshots().Save(t.Context(), f.store.Querier(), f.snapshot(t)))

	_, _, err := f.service.LatestSnapshot(t.Context(), reporting.Actor{}, testNow)
	require.ErrorIs(t, err, apperr.ErrForbidden)
}

func TestLatestSnapshotWithoutAnyMarketData(t *testing.T) {
	t.Parallel()

	f := newFixture(t)

	_, _, err := f.service.LatestSnapshot(t.Context(), f.issuer, testNow)
	require.ErrorIs(t, err, apperr.ErrNotFound)
}

// TestTimelineIsScopedLikeTheInvoice keeps a receivable's history as private as the
// receivable, and reports its absence the same way.
func TestTimelineIsScopedLikeTheInvoice(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	inv, _, _ := f.assessedInvoice(t, f.issuer.OrganizationID)

	require.NoError(t, f.store.Audit().Record(t.Context(), f.store.Querier(),
		audit.Of(t.Context(), f.issuer.OrganizationID.String(), invoice.ActionCreated,
			invoice.EntityType, inv.ID.String(), testNow).
			With("status", invoice.StatusDraft.String())))
	require.NoError(t, f.store.Audit().Record(t.Context(), f.store.Querier(),
		audit.Of(t.Context(), audit.SystemActor, "invoice.assessed",
			invoice.EntityType, inv.ID.String(), testNow.Add(time.Minute)).
			With("grade", "B")))

	events, err := f.service.TimelineFor(t.Context(), f.issuer, inv.ID, 0)
	require.NoError(t, err)
	require.Len(t, events, 2)
	assert.Equal(t, "invoice.assessed", events[0].Action, "newest first")

	limited, err := f.service.TimelineFor(t.Context(), f.operator, inv.ID, 1)
	require.NoError(t, err)
	assert.Len(t, limited, 1)

	_, err = f.service.TimelineFor(t.Context(), f.stranger, inv.ID, 0)
	require.ErrorIs(t, err, apperr.ErrNotFound)

	_, err = f.service.TimelineFor(t.Context(), f.issuer, uuid.New(), 0)
	require.ErrorIs(t, err, apperr.ErrNotFound)
}

// TestTimelineWithoutARecorder reports honestly rather than answering with an empty
// history, which would read as "nothing ever happened".
func TestTimelineWithoutARecorder(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	inv, _, _ := f.assessedInvoice(t, f.issuer.OrganizationID)

	service := reporting.NewService(reporting.Config{
		DB: f.store, Invoices: f.store.Invoices(), Assessments: f.store.Assessments(),
		Snapshots: f.store.Snapshots(), Market: marketdata.DemoQuery(),
	})

	_, err := service.TimelineFor(t.Context(), f.issuer, inv.ID, 0)
	require.ErrorIs(t, err, apperr.ErrUnavailable)
}
