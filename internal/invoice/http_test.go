package invoice_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/skimer2king/factorflow/internal/invoice"
	"github.com/skimer2king/factorflow/internal/platform/httpserver"
)

// apiFixture wires the handler behind the real router and middleware, so a test exercises
// the same path a client does, including authentication and problem documents.
type apiFixture struct {
	*serviceFixture
	router http.Handler
	actor  httpserver.Actor
}

func newAPIFixture(t *testing.T) *apiFixture {
	t.Helper()

	sf := newServiceFixture(t)
	f := &apiFixture{
		serviceFixture: sf,
		actor:          httpserver.Actor{OrganizationID: sf.issuer.OrganizationID, Role: httpserver.RoleOwner},
	}

	handler := invoice.NewHandler(sf.service)
	f.router = httpserver.NewRouter(httpserver.Dependencies{
		Routes: func(r chi.Router) {
			r.Use(httpserver.Authenticate(httpserver.ResolverFunc(func(*http.Request) (httpserver.Actor, error) {
				return f.actor, nil
			})))
			r.Group(func(protected chi.Router) {
				protected.Use(httpserver.RequireActor)
				handler.Routes(protected)
			})
		},
	})
	return f
}

func (f *apiFixture) request(t *testing.T, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()

	var reader io.Reader = http.NoBody
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	return rec
}

func (f *apiFixture) createInvoice(t *testing.T, number string) map[string]any {
	t.Helper()

	body := fmt.Sprintf(`{
		"debtor_ref": "ACME Logistics GmbH",
		"number": %q,
		"face": "10000.00",
		"currency": "USD",
		"issued_at": %q,
		"due_at": %q
	}`, number, testIssuedAt.Format(time.RFC3339), testDueAt.Format(time.RFC3339))

	rec := f.request(t, http.MethodPost, "/api/v1/invoices", body)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	return decodeBody(t, rec)
}

func decodeBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()

	var out map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	return out
}

func TestCreateInvoiceEndpoint(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	created := f.createInvoice(t, "INV-2026-0042")

	assert.Equal(t, "DRAFT", created["status"])
	assert.Equal(t, "10000.00", created["face"], "money is a decimal string, never a JSON number")
	assert.Equal(t, "USD", created["currency"])
	assert.Equal(t, float64(60), created["tenor_days"])
	assert.Equal(t, f.actor.OrganizationID.String(), created["issuer_id"])
	assert.NotContains(t, created, "assessment_id", "an unassessed invoice omits the field")
}

func TestCreateInvoiceValidation(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)

	tests := []struct {
		name       string
		body       string
		wantStatus int
		wantFields []string
	}{
		{
			name: "unknown currency",
			body: `{"debtor_ref":"ACME","number":"INV-1","face":"100.00","currency":"XYZ",
			        "issued_at":"2026-09-05T12:00:00Z","due_at":"2026-11-04T12:00:00Z"}`,
			wantStatus: http.StatusUnprocessableEntity,
			wantFields: []string{"currency"},
		},
		{
			name: "sub-minor precision",
			body: `{"debtor_ref":"ACME","number":"INV-1","face":"100.005","currency":"USD",
			        "issued_at":"2026-09-05T12:00:00Z","due_at":"2026-11-04T12:00:00Z"}`,
			wantStatus: http.StatusUnprocessableEntity,
			wantFields: []string{"face"},
		},
		{
			name: "bad timestamps",
			body: `{"debtor_ref":"ACME","number":"INV-1","face":"100.00","currency":"USD",
			        "issued_at":"yesterday","due_at":""}`,
			wantStatus: http.StatusUnprocessableEntity,
			wantFields: []string{"issued_at", "due_at"},
		},
		{
			name: "domain invariants",
			body: `{"debtor_ref":"","number":"","face":"0.00","currency":"USD",
			        "issued_at":"2026-09-05T12:00:00Z","due_at":"2026-11-04T12:00:00Z"}`,
			wantStatus: http.StatusUnprocessableEntity,
			wantFields: []string{"debtor_ref", "number", "face"},
		},
		{
			name:       "unknown field",
			body:       `{"debtor_ref":"ACME","surprise":true}`,
			wantStatus: http.StatusBadRequest,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := f.request(t, http.MethodPost, "/api/v1/invoices", tc.body)
			require.Equal(t, tc.wantStatus, rec.Code, rec.Body.String())

			if len(tc.wantFields) == 0 {
				return
			}
			var problem httpserver.Problem
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &problem))

			fields := make([]string, 0, len(problem.Violations))
			for _, v := range problem.Violations {
				fields = append(fields, v.Field)
			}
			for _, want := range tc.wantFields {
				assert.Containsf(t, fields, want, "violations: %v", fields)
			}
		})
	}
}

// TestUploadAndAssessEndpoints walks the demo's first leg: encrypted upload, then a queued
// confidential assessment.
func TestUploadAndAssessEndpoints(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	created := f.createInvoice(t, "INV-2026-0042")
	id := created["id"].(string)

	docBody := fmt.Sprintf(`{
		"object_key": "invoices/2026/09/%s.enc",
		"cipher_hash": %q,
		"key_ref": "cre-secret://data-key/01J8Z9",
		"mime": "application/pdf",
		"size_bytes": 482113
	}`, id, testCipherHash)

	rec := f.request(t, http.MethodPost, "/api/v1/invoices/"+id+"/document", docBody)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "UPLOADED", decodeBody(t, rec)["status"])

	rec = f.request(t, http.MethodGet, "/api/v1/invoices/"+id+"/document", "")
	require.Equal(t, http.StatusOK, rec.Code)
	document := decodeBody(t, rec)
	assert.Equal(t, testCipherHash, document["cipher_hash"])
	assert.NotContains(t, document, "key_ref",
		"the key reference belongs to the confidential workflow, not to an API response")

	rec = f.request(t, http.MethodPost, "/api/v1/invoices/"+id+"/assess", "")
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	assert.Equal(t, "EXTRACTING", decodeBody(t, rec)["status"])
	assert.True(t, f.db.querier.published("outbox_events"))
}

func TestRejectEndpoint(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	id := f.createInvoice(t, "INV-2026-0042")["id"].(string)

	rec := f.request(t, http.MethodPost, "/api/v1/invoices/"+id+"/reject", `{"reason":"withdrawn by issuer"}`)
	require.Equal(t, http.StatusOK, rec.Code)

	body := decodeBody(t, rec)
	assert.Equal(t, "REJECTED", body["status"])
	assert.Equal(t, "withdrawn by issuer", body["reason"])

	rec = f.request(t, http.MethodPost, "/api/v1/invoices/"+id+"/reject", `{"reason":"again"}`)
	assert.Equal(t, http.StatusConflict, rec.Code, "a terminal invoice does not move")
}

func TestApproveWithoutAnAssessmentIsAConflict(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	id := f.createInvoice(t, "INV-2026-0042")["id"].(string)

	rec := f.request(t, http.MethodPost, "/api/v1/invoices/"+id+"/approve", "")
	assert.Equal(t, http.StatusConflict, rec.Code)
}

func TestListEndpoint(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	f.createInvoice(t, "INV-1")
	f.createInvoice(t, "INV-2")

	rec := f.request(t, http.MethodGet, "/api/v1/invoices", "")
	require.Equal(t, http.StatusOK, rec.Code)

	var body struct {
		Items []map[string]any `json:"items"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Len(t, body.Items, 2)

	rec = f.request(t, http.MethodGet, "/api/v1/invoices?limit=0", "")
	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)

	rec = f.request(t, http.MethodGet, "/api/v1/invoices?limit=abc", "")
	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
}

func TestMalformedInvoiceID(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)

	rec := f.request(t, http.MethodGet, "/api/v1/invoices/not-a-uuid", "")
	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
}

func TestAnonymousRequestsAreRejected(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	created := f.createInvoice(t, "INV-2026-0042")
	f.actor = httpserver.Actor{}

	for _, path := range []string{
		"/api/v1/invoices",
		"/api/v1/invoices/" + created["id"].(string),
	} {
		rec := f.request(t, http.MethodGet, path, "")
		assert.Equalf(t, http.StatusUnauthorized, rec.Code, "path %s", path)
	}
}

// TestAnotherOrganizationGetsNotFound repeats the disclosure rule at the transport level:
// the status code itself must not confirm that someone else's invoice exists.
func TestAnotherOrganizationGetsNotFound(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	created := f.createInvoice(t, "INV-2026-0042")

	f.actor = httpserver.Actor{OrganizationID: uuid.New(), Role: httpserver.RoleOwner}
	rec := f.request(t, http.MethodGet, "/api/v1/invoices/"+created["id"].(string), "")

	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.NotEqual(t, http.StatusForbidden, rec.Code)
}
