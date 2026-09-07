package reporting_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoldFridge/factorflow/internal/risk"
)

func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()

	var out map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	return out
}

// TestAssessmentEndpointRendersEveryNumberAsAString is the wire contract that keeps exact
// arithmetic exact: a JSON number would have rounded the price before the client read it.
func TestAssessmentEndpointRendersEveryNumberAsAString(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	inv, assessment, snapshot := f.assessedInvoice(t, f.issuer.OrganizationID)

	rec := f.get(t, "/api/v1/invoices/"+inv.ID.String()+"/assessment", "issuer")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	body := decode(t, rec)
	assert.Equal(t, assessment.ID.String(), body["id"])
	assert.Equal(t, inv.ID.String(), body["invoice_id"])
	assert.Equal(t, assessment.ModelVersion, body["model_version"])
	assert.Equal(t, assessment.Grade.String(), body["grade"])
	assert.Equal(t, assessment.PD.String(), body["pd"])
	assert.Equal(t, assessment.ReservePrice.String(), body["reserve_price"])
	assert.Equal(t, "USD", body["currency"])
	assert.Equal(t, assessment.ConfidentialCommitment, body["confidential_commitment"])
	assert.Equal(t, assessment.ConfidentialNonce, body["confidential_nonce"],
		"a commitment nobody can recompute proves nothing")

	premiums := body["premiums"].(map[string]any)
	assert.Equal(t, assessment.Premiums.Total().String(), premiums["total"])

	// The features are keyed by the same names the contributions use, so the two line up.
	features := body["features"].(map[string]any)
	require.Len(t, features, len(risk.FeatureNames()))
	for _, name := range risk.FeatureNames() {
		assert.Contains(t, features, name)
	}

	contributions := body["contributions"].([]any)
	require.Len(t, contributions, len(risk.FeatureNames()))
	first := contributions[0].(map[string]any)
	assert.Equal(t, assessment.RankedContributions()[0].Feature, first["feature"])
	assert.Equal(t, assessment.RankedContributions()[0].Effect.String(), first["effect"])

	market := body["market_snapshot"].(map[string]any)
	assert.Equal(t, snapshot.PayloadHash, market["payload_hash"])
	assert.Equal(t, snapshot.Benchmark.String(), market["benchmark_apr"])
	assert.Equal(t, float64(len(snapshot.Markets)), market["market_count"])
	assert.True(t, market["fresh"].(bool))
}

func TestAssessmentEndpointAuthorization(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	inv, _, _ := f.assessedInvoice(t, f.issuer.OrganizationID)
	path := "/api/v1/invoices/" + inv.ID.String() + "/assessment"

	tests := []struct {
		name       string
		path       string
		actor      string
		wantStatus int
	}{
		{name: "issuer", path: path, actor: "issuer", wantStatus: http.StatusOK},
		{name: "operator", path: path, actor: "operator", wantStatus: http.StatusOK},
		{name: "stranger", path: path, actor: "stranger", wantStatus: http.StatusNotFound},
		{name: "anonymous", path: path, actor: "", wantStatus: http.StatusNotFound},
		{name: "unknown invoice", path: "/api/v1/invoices/" + uuid.NewString() + "/assessment", actor: "issuer", wantStatus: http.StatusNotFound},
		{name: "malformed id", path: "/api/v1/invoices/not-a-uuid/assessment", actor: "issuer", wantStatus: http.StatusUnprocessableEntity},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := f.get(t, tc.path, tc.actor)
			assert.Equal(t, tc.wantStatus, rec.Code, rec.Body.String())
		})
	}
}

// TestBenchmarkEndpointPublishesProvenance checks that the market view carries where the
// numbers came from, not just the numbers.
func TestBenchmarkEndpointPublishesProvenance(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	snapshot := f.snapshot(t)
	require.NoError(t, f.store.Snapshots().Save(t.Context(), f.store.Querier(), snapshot))

	rec := f.get(t, "/api/v1/market/benchmarks/latest", "issuer")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	body := decode(t, rec)
	assert.Equal(t, snapshot.PayloadHash, body["payload_hash"])
	assert.Equal(t, snapshot.QueryHash, body["query_hash"])
	assert.Equal(t, snapshot.Provider, body["provider"])
	assert.Equal(t, snapshot.Network, body["network"])
	assert.Equal(t, snapshot.Benchmark.String(), body["benchmark_apr"])
	assert.Equal(t, snapshot.Volatility.String(), body["volatility"])
	assert.Equal(t, snapshot.TotalLiquidity.String(), body["total_liquidity"])
	assert.NotEmpty(t, body["subgraph_ids"])
	assert.NotEmpty(t, body["block_numbers"])
	assert.Equal(t, snapshot.ExpiresAt().Format(time.RFC3339), body["expires_at"])
	assert.True(t, body["fresh"].(bool))
}

// TestBenchmarkEndpointServesAStaleSnapshot proves the endpoint reports rather than
// enforces freshness: a trader may still see the last market, marked as expired.
func TestBenchmarkEndpointServesAStaleSnapshot(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	snapshot := f.snapshot(t)
	require.NoError(t, f.store.Snapshots().Save(t.Context(), f.store.Querier(), snapshot))
	f.clock = snapshot.ExpiresAt().Add(time.Minute)

	rec := f.get(t, "/api/v1/market/benchmarks/latest", "issuer")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.False(t, decode(t, rec)["fresh"].(bool))
}

func TestBenchmarkEndpointRequiresACaller(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	require.NoError(t, f.store.Snapshots().Save(t.Context(), f.store.Querier(), f.snapshot(t)))

	assert.Equal(t, http.StatusForbidden, f.get(t, "/api/v1/market/benchmarks/latest", "").Code)
}

func TestBenchmarkEndpointWithoutMarketData(t *testing.T) {
	t.Parallel()

	f := newFixture(t)

	assert.Equal(t, http.StatusNotFound, f.get(t, "/api/v1/market/benchmarks/latest", "issuer").Code)
}
