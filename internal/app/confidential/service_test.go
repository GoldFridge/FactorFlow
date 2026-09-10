package confidential_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoldFridge/factorflow/internal/app/confidential"
	"github.com/GoldFridge/factorflow/internal/invoice"
	"github.com/GoldFridge/factorflow/internal/platform/httpserver"
	"github.com/GoldFridge/factorflow/internal/platform/money"
	"github.com/GoldFridge/factorflow/internal/platform/objects"
	"github.com/GoldFridge/factorflow/internal/risk"
	"github.com/GoldFridge/factorflow/internal/testsupport/memrepo"
)

var testNow = time.Date(2026, time.September, 10, 9, 0, 0, 0, time.UTC)

const token = "a-token-the-vault-released"

// delivered records what the platform accepted, standing in for the scoring path.
type delivered struct {
	invoiceID uuid.UUID
	result    risk.WorkflowResult
	err       error
	calls     int
}

func (d *delivered) Complete(_ context.Context, invoiceID uuid.UUID, result risk.WorkflowResult) error {
	d.calls++
	d.invoiceID, d.result = invoiceID, result
	return d.err
}

type fixture struct {
	store    *memrepo.Store
	assessor *delivered
	router   http.Handler
	invoice  *invoice.Invoice
	document *invoice.Document
}

func newFixture(t *testing.T) *fixture {
	t.Helper()

	store := memrepo.New()
	f := &fixture{store: store, assessor: &delivered{}}

	service := confidential.NewService(confidential.Config{
		DB:       store,
		Invoices: store.Invoices(),
		Objects:  store.Objects(),
		Assessor: f.assessor,
		Now:      func() time.Time { return testNow },
	})

	handler := confidential.NewHandler(service, token)
	require.True(t, handler.Enabled())

	f.router = httpserver.NewRouter(httpserver.Dependencies{
		Routes: func(r chi.Router) { handler.Routes(r) },
	})
	return f
}

// waiting puts one invoice in the state a confidential run collects: extracting, with an
// encrypted document behind it.
func (f *fixture) waiting(t *testing.T, ciphertext []byte) {
	t.Helper()

	inv, err := invoice.New(invoice.NewParams{
		ID:        uuid.MustParse("44444444-4444-4444-8444-444444444444"),
		IssuerID:  uuid.New(),
		DebtorRef: "ACME Logistics GmbH",
		Number:    "INV-2026-0009",
		Face:      money.MustParse("15000.00", money.USD),
		IssuedAt:  testNow.Add(-24 * time.Hour),
		DueAt:     testNow.Add(60 * 24 * time.Hour),
	}, testNow)
	require.NoError(t, err)

	object, err := objects.New(inv.ID, ciphertext, testNow)
	require.NoError(t, err)
	require.NoError(t, f.store.Objects().Put(context.Background(), f.store.Querier(), object))

	document, err := invoice.NewDocument(invoice.NewDocumentParams{
		InvoiceID:  inv.ID,
		ObjectKey:  object.Key,
		CipherHash: object.CipherHash,
		KeyRef:     "vault://factorflow/document-key",
		MIME:       "application/json",
		SizeBytes:  object.SizeBytes,
	}, testNow)
	require.NoError(t, err)

	require.NoError(t, inv.MarkUploaded(testNow))
	require.NoError(t, inv.StartAssessment(testNow))
	f.store.PutInvoice(inv)
	require.NoError(t, f.store.Invoices().SaveDocument(context.Background(), f.store.Querier(), document))

	f.invoice, f.document = inv, document
}

func (f *fixture) call(t *testing.T, method, path, bearer, body string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	return rec
}

/*
 * TestWorkIsHandedOverWithoutAKey is the property the whole confidential design rests on.
 * The workflow is given the ciphertext and the digest, and there is no field, header or
 * second request that would produce the key — the platform does not have it.
 */
func TestWorkIsHandedOverWithoutAKey(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.waiting(t, []byte("sealed bytes nobody here can read"))

	rec := f.call(t, http.MethodGet, "/api/v1/confidential/work", token, "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var work map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &work))

	assert.Equal(t, f.invoice.ID.String(), work["invoice_id"])
	assert.Equal(t, f.document.CipherHash, work["cipher_hash"])
	assert.Equal(t, risk.FeatureSchemaV1, work["schema_version"])

	ciphertext, err := base64.StdEncoding.DecodeString(work["ciphertext"].(string))
	require.NoError(t, err)
	assert.Equal(t, "sealed bytes nobody here can read", string(ciphertext))

	// The nonce is derived, so the enclave arrives at the same one without being told.
	assert.Equal(t, risk.DeriveNonce(f.invoice.ID, f.document.CipherHash), work["nonce"])

	for _, forbidden := range []string{"key", "data_key", "secret", "plaintext"} {
		assert.NotContains(t, work, forbidden)
	}
}

// TestNothingToDoSaysSo: an idle platform answers no content, which the workflow reads as
// "nothing to assess" rather than as a failure worth retrying.
func TestNothingToDoSaysSo(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	assert.Equal(t, http.StatusNoContent,
		f.call(t, http.MethodGet, "/api/v1/confidential/work", token, "").Code)
}

/*
 * TestOnlyTheWorkflowMayCollect. The endpoint hands out documents, and although they are
 * encrypted, a stranger collecting them learns which receivables exist and when. The token
 * is the gate, and a wrong one is refused the same way as none at all.
 */
func TestOnlyTheWorkflowMayCollect(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.waiting(t, []byte("sealed"))

	for _, bearer := range []string{"", "wrong-token", token + "x"} {
		rec := f.call(t, http.MethodGet, "/api/v1/confidential/work", bearer, "")
		assert.Equal(t, http.StatusForbidden, rec.Code, "bearer %q", bearer)
	}
}

// TestAnAnswerIsTakenBack covers the return leg: the features reach the scoring path in the
// domain's own types.
func TestAnAnswerIsTakenBack(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.waiting(t, []byte("sealed"))

	body := `{
		"invoice_id": "` + f.invoice.ID.String() + `",
		"features": {
			"dso_norm": "0.500000", "late_payment_rate": "0.125000", "dispute_flag": "0.000000",
			"debtor_concentration": "0.400000", "market_volatility": "0.000000", "debtor_risk": "0.200000"
		},
		"confidence": "0.910000",
		"arithmetic_valid": true,
		"mitigations": {"recourse": true, "collateralized": false},
		"commitment": "0x` + strings.Repeat("a", 64) + `",
		"schema_version": "` + risk.FeatureSchemaV1 + `",
		"model_version": "cre-invoice-risk-v1",
		"evidence": "cre workflow simulate"
	}`

	rec := f.call(t, http.MethodPost, "/api/v1/confidential/results", token, body)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())

	require.Equal(t, 1, f.assessor.calls)
	assert.Equal(t, f.invoice.ID, f.assessor.invoiceID)
	assert.Equal(t, "0.5", f.assessor.result.Features.DSONorm.String())
	assert.True(t, f.assessor.result.ArithmeticValid)
	assert.True(t, f.assessor.result.Mitigations.Recourse)
	assert.Equal(t, "cre-invoice-risk-v1", f.assessor.result.ModelVersion)
}

/*
 * TestAnAnswerIsUntrustedInput. This arrives over HTTP from a system the platform does not
 * run, and every number in it goes straight into a price. A feature that is not a number is
 * refused at the boundary rather than parsed into a zero that would quietly make a
 * receivable look safe.
 */
func TestAnAnswerIsUntrustedInput(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.waiting(t, []byte("sealed"))

	cases := []struct {
		name string
		want int
		body string
	}{
		{"a feature that is not a number", http.StatusUnprocessableEntity, `{"invoice_id":"` + uuid.New().String() +
			`","features":{"dso_norm":"quite high","late_payment_rate":"0.1","dispute_flag":"0",
			"debtor_concentration":"0.1","market_volatility":"0","debtor_risk":"0.1"},
			"confidence":"0.9","commitment":"0x` + strings.Repeat("a", 64) + `"}`},
		{"no invoice at all", http.StatusUnprocessableEntity, `{"invoice_id":"","features":{"dso_norm":"0.1",
			"late_payment_rate":"0.1","dispute_flag":"0","debtor_concentration":"0.1",
			"market_volatility":"0","debtor_risk":"0.1"},"confidence":"0.9"}`},
		// A body with fields nobody defined is refused before it is interpreted: strict
		// decoding is what stops a typo in a field name becoming a silent zero.
		{"nothing that resembles a result", http.StatusBadRequest, `{"hello":"world"}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			rec := f.call(t, http.MethodPost, "/api/v1/confidential/results", token, tc.body)
			assert.Equal(t, tc.want, rec.Code, rec.Body.String())
		})
	}
}

// TestWithoutATokenTheEndpointsDoNotExist: a deployment with no confidential workflow does
// not carry a route that hands out documents, however well guarded it would be.
func TestWithoutATokenTheEndpointsDoNotExist(t *testing.T) {
	t.Parallel()

	handler := confidential.NewHandler(nil, "  ")
	assert.False(t, handler.Enabled())
}
