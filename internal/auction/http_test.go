package auction_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoldFridge/factorflow/internal/auction"
	"github.com/GoldFridge/factorflow/internal/platform/httpserver"
	"github.com/GoldFridge/factorflow/internal/risk"
)

// apiFixture wires the handler behind the real router, so a test exercises the same path a
// client does, including authentication and problem documents.
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
		actor: httpserver.Actor{
			OrganizationID: sf.investor.OrganizationID,
			Role:           httpserver.RoleOwner,
			Eligible:       true,
		},
	}

	handler := auction.NewHandler(sf.service)
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

func (f *apiFixture) as(actor auction.Actor) {
	f.actor = httpserver.Actor{
		OrganizationID: actor.OrganizationID,
		Role:           httpserver.RoleOwner,
		Eligible:       actor.Eligible,
		Operator:       actor.Operator,
	}
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

func decodeBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()

	var out map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	return out
}

const validBidBody = `{
	"budget": "50000.00",
	"currency": "USD",
	"min_yield": "0.08",
	"max_grade": "C",
	"max_tenor_days": 120,
	"minimum_lot": "1000.00",
	"max_issuer_share": "0.40",
	"max_debtor_share": "0.25",
	"max_grade_share": {"C": "0.30"}
}`

// TestMarketplaceEndpoints walks the investor's path: see the batch and bid on it. The
// clearing endpoint belongs to the application layer, because it moves invoices too, and is
// tested there.
func TestMarketplaceEndpoints(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	a := f.openAuction(t)

	rec := f.request(t, http.MethodGet, "/api/v1/auctions?status=OPEN", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var listed struct {
		Items []map[string]any `json:"items"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &listed))
	require.Len(t, listed.Items, 1)
	assert.Equal(t, "10000.00", listed.Items[0]["total_supply"])

	lots := listed.Items[0]["lots"].([]any)
	require.Len(t, lots, 1)
	first := lots[0].(map[string]any)
	assert.Equal(t, "9755.32", first["reserve_price"])
	assert.Equal(t, "0.152580", first["implied_yield"],
		"the number an investor decides on is shown beside the price")

	rec = f.request(t, http.MethodPost, "/api/v1/auctions/"+a.ID.String()+"/bids", validBidBody)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	bid := decodeBody(t, rec)
	assert.Equal(t, "ACTIVE", bid["status"])
	assert.Equal(t, "50000.00", bid["budget"])
	assert.Equal(t, "0.080000", bid["min_yield"])
}

func TestBidValidation(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	a := f.openAuction(t)
	path := "/api/v1/auctions/" + a.ID.String() + "/bids"

	tests := []struct {
		name       string
		body       string
		wantStatus int
		wantFields []string
	}{
		{
			name:       "unknown currency",
			body:       `{"budget":"100.00","currency":"XYZ","min_yield":"0.05","max_grade":"C","max_tenor_days":90}`,
			wantStatus: http.StatusUnprocessableEntity,
			wantFields: []string{"currency"},
		},
		{
			name:       "unknown grade and malformed yield",
			body:       `{"budget":"100.00","currency":"USD","min_yield":"eight percent","max_grade":"Z","max_tenor_days":90}`,
			wantStatus: http.StatusUnprocessableEntity,
			wantFields: []string{"min_yield", "max_grade"},
		},
		{
			name:       "share out of range",
			body:       `{"budget":"100.00","currency":"USD","min_yield":"0.05","max_grade":"C","max_tenor_days":90,"max_issuer_share":"1.5"}`,
			wantStatus: http.StatusUnprocessableEntity,
			wantFields: []string{"max_issuer_share"},
		},
		{
			name:       "sub-minor budget",
			body:       `{"budget":"100.005","currency":"USD","min_yield":"0.05","max_grade":"C","max_tenor_days":90}`,
			wantStatus: http.StatusUnprocessableEntity,
			wantFields: []string{"budget"},
		},
		{
			name:       "unknown field",
			body:       `{"budget":"100.00","currency":"USD","min_yield":"0.05","max_grade":"C","max_tenor_days":90,"leverage":3}`,
			wantStatus: http.StatusBadRequest,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := f.request(t, http.MethodPost, path, tc.body)
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

func TestOmittedOptionalBidFieldsMeanNoLimit(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	a := f.openAuction(t)

	minimal := `{"budget":"50000.00","currency":"USD","min_yield":"0.05","max_grade":"C","max_tenor_days":120}`
	rec := f.request(t, http.MethodPost, "/api/v1/auctions/"+a.ID.String()+"/bids", minimal)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	bid := decodeBody(t, rec)
	assert.Equal(t, "0.00", bid["minimum_lot"], "no minimum lot means any size is acceptable")
	assert.Equal(t, "0.000000", bid["max_issuer_share"], "an omitted share is uncapped")
}

func TestCancelBidEndpoint(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	a := f.openAuction(t)

	rec := f.request(t, http.MethodPost, "/api/v1/auctions/"+a.ID.String()+"/bids", validBidBody)
	require.Equal(t, http.StatusCreated, rec.Code)
	bidID := decodeBody(t, rec)["id"].(string)

	rec = f.request(t, http.MethodPost, "/api/v1/bids/"+bidID+"/cancel", "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "CANCELLED", decodeBody(t, rec)["status"])

	f.as(f.other)
	rec = f.request(t, http.MethodPost, "/api/v1/bids/"+bidID+"/cancel", "")
	assert.Equal(t, http.StatusNotFound, rec.Code, "someone else's bid is invisible")
}

func TestIssuerOnlyEndpoints(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	a := f.openAuction(t)
	f.clock = testClose

	rec := f.request(t, http.MethodPost, "/api/v1/auctions/"+a.ID.String()+"/open", "")
	assert.Equal(t, http.StatusForbidden, rec.Code)

	rec = f.request(t, http.MethodPost, "/api/v1/auctions/"+a.ID.String()+"/cancel", `{"reason":"not mine"}`)
	assert.Equal(t, http.StatusForbidden, rec.Code)
}

func TestAnonymousAndMalformedRequests(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	a := f.openAuction(t)

	rec := f.request(t, http.MethodGet, "/api/v1/auctions/not-a-uuid", "")
	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)

	rec = f.request(t, http.MethodGet, "/api/v1/auctions?limit=0", "")
	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)

	f.actor = httpserver.Actor{}
	for _, path := range []string{
		"/api/v1/auctions",
		"/api/v1/auctions/" + a.ID.String(),
		"/api/v1/auctions/" + a.ID.String() + "/allocations",
	} {
		rec := f.request(t, http.MethodGet, path, "")
		assert.Equalf(t, http.StatusUnauthorized, rec.Code, "path %s", path)
	}
}

func TestBidsEndpointShowsOnlyWhatTheCallerMaySee(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	a := f.openAuction(t)

	require.Equal(t, http.StatusCreated,
		f.request(t, http.MethodPost, "/api/v1/auctions/"+a.ID.String()+"/bids", validBidBody).Code)

	f.as(f.other)
	require.Equal(t, http.StatusCreated,
		f.request(t, http.MethodPost, "/api/v1/auctions/"+a.ID.String()+"/bids", validBidBody).Code)

	rec := f.request(t, http.MethodGet, "/api/v1/auctions/"+a.ID.String()+"/bids", "")
	require.Equal(t, http.StatusOK, rec.Code)

	var mine struct {
		Items []map[string]any `json:"items"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &mine))
	assert.Len(t, mine.Items, 1, "an investor sees only its own bid")

	f.as(f.issuer)
	rec = f.request(t, http.MethodGet, "/api/v1/auctions/"+a.ID.String()+"/bids", "")
	require.Equal(t, http.StatusOK, rec.Code)

	var all struct {
		Items []map[string]any `json:"items"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &all))
	assert.Len(t, all.Items, 2, "the issuer sees the batch it is about to clear")
}

func TestGradeAndLotShapes(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	a := f.openAuction(t, lot(4, "DELTA", issuerA(), "12000.00", "11600.00", risk.GradeD, 90))

	rec := f.request(t, http.MethodGet, "/api/v1/auctions/"+a.ID.String(), "")
	require.Equal(t, http.StatusOK, rec.Code)

	body := decodeBody(t, rec)
	assert.Equal(t, "USD", body["currency"])
	assert.Equal(t, fmt.Sprintf("%d", 1), fmt.Sprintf("%d", len(body["lots"].([]any))))

	first := body["lots"].([]any)[0].(map[string]any)
	assert.Equal(t, "D", first["grade"])
	assert.Equal(t, float64(90), first["tenor_days"])
	assert.NotEmpty(t, first["asset_id"])
	assert.NotEqual(t, uuid.Nil.String(), first["invoice_id"])
}
