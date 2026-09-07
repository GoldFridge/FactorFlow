package marketplace_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoldFridge/factorflow/internal/app/marketplace"
	"github.com/GoldFridge/factorflow/internal/platform/httpserver"
	"github.com/GoldFridge/factorflow/internal/settlement"
)

// router puts the handler behind the real router, with the caller chosen by a header in
// place of the session middleware.
func (f *clearedFixture) router(t *testing.T) http.Handler {
	t.Helper()

	handler := marketplace.NewHandler(f.service)
	return httpserver.NewRouter(httpserver.Dependencies{
		Routes: func(r chi.Router) {
			r.Use(func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					actor := httpserver.Actor{Eligible: true}
					switch req.Header.Get("X-Test-Actor") {
					case "issuer":
						actor.OrganizationID = f.issuer.OrganizationID
					case "other":
						actor.OrganizationID = f.other.OrganizationID
					}
					next.ServeHTTP(w, req.WithContext(httpserver.ContextWithActor(req.Context(), actor)))
				})
			})
			handler.Routes(r)
		},
	})
}

func do(t *testing.T, router http.Handler, method, path, actor string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(method, path, http.NoBody)
	if actor != "" {
		req.Header.Set("X-Test-Actor", actor)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func decodeItems(t *testing.T, rec *httptest.ResponseRecorder) []any {
	t.Helper()

	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	return body["items"].([]any)
}

// TestSettleEndpointAcceptsRatherThanConfirms is the honest status for this operation: the
// transfers are queued, and claiming 200 would say the assets had already moved.
func TestSettleEndpointAcceptsRatherThanConfirms(t *testing.T) {
	t.Parallel()

	f := newClearedFixture(t)
	router := f.router(t)

	rec := do(t, router, http.MethodPost, "/api/v1/auctions/"+f.auction.ID.String()+"/settle", "issuer")
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())

	items := decodeItems(t, rec)
	require.Len(t, items, 1)

	planned := items[0].(map[string]any)
	assert.Equal(t, settlement.StatePrepared.String(), planned["state"])
	assert.Equal(t, f.invoice.ID.String(), planned["invoice_id"])
	assert.NotEmpty(t, planned["operation_id"])
	assert.NotContains(t, planned, "tx_id", "nothing has been submitted yet")
}

func TestSettlementsEndpointShowsTheSagaState(t *testing.T) {
	t.Parallel()

	f := newClearedFixture(t)
	router := f.router(t)

	planned, err := f.service.SettleAuction(t.Context(), f.issuer, f.auction.ID, "trace-1")
	require.NoError(t, err)
	require.NoError(t, f.worker.Handle(t.Context(), settleEvent(t, planned[0].ID)))

	rec := do(t, router, http.MethodGet, "/api/v1/auctions/"+f.auction.ID.String()+"/settlements", "issuer")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	item := decodeItems(t, rec)[0].(map[string]any)
	assert.Equal(t, settlement.StateAccounted.String(), item["state"])
	assert.NotEmpty(t, item["tx_id"], "the transaction a participant can look up")
	assert.Equal(t, float64(1), item["attempts"])
	assert.Equal(t, "10000.00", item["notional"])
	assert.Equal(t, "USD", item["currency"])
}

func TestSettlementEndpointsAuthorization(t *testing.T) {
	t.Parallel()

	f := newClearedFixture(t)
	router := f.router(t)
	path := "/api/v1/auctions/" + f.auction.ID.String()

	tests := []struct {
		name       string
		method     string
		path       string
		actor      string
		wantStatus int
	}{
		{name: "another issuer settles", method: http.MethodPost, path: path + "/settle", actor: "other", wantStatus: http.StatusForbidden},
		{name: "another issuer reads", method: http.MethodGet, path: path + "/settlements", actor: "other", wantStatus: http.StatusForbidden},
		{name: "unknown auction", method: http.MethodPost, path: "/api/v1/auctions/" + uuid.NewString() + "/settle", actor: "issuer", wantStatus: http.StatusNotFound},
		{name: "malformed id", method: http.MethodGet, path: "/api/v1/auctions/not-a-uuid/settlements", actor: "issuer", wantStatus: http.StatusUnprocessableEntity},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(t, router, tc.method, tc.path, tc.actor)
			assert.Equal(t, tc.wantStatus, rec.Code, rec.Body.String())
		})
	}
}
