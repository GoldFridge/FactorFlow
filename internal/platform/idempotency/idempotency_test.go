package idempotency_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/skimer2king/factorflow/internal/organization"
	"github.com/skimer2king/factorflow/internal/platform/apperr"
	"github.com/skimer2king/factorflow/internal/platform/httpserver"
	"github.com/skimer2king/factorflow/internal/platform/idempotency"
	"github.com/skimer2king/factorflow/internal/platform/pgtest"
	"github.com/skimer2king/factorflow/internal/platform/postgres"
)

var testNow = time.Date(2026, time.September, 5, 12, 0, 0, 0, time.UTC)

func TestMain(m *testing.M) {
	code := m.Run()
	pgtest.Shutdown()
	os.Exit(code)
}

// seedOrganization inserts the organization an idempotency key is scoped to.
func seedOrganization(t *testing.T, db *postgres.DB, seq int) uuid.UUID {
	t.Helper()

	org, err := organization.New(organization.NewParams{
		ID:     uuid.New(),
		Type:   organization.TypeIssuer,
		Name:   fmt.Sprintf("Issuer %d", seq),
		Wallet: fmt.Sprintf("0x%040x", seq),
	}, testNow)
	require.NoError(t, err)
	require.NoError(t, organization.NewPostgresRepository().Create(context.Background(), db.Querier(), org))
	return org.ID
}

// harness wires the middleware in front of a counting handler, which is how a test tells a
// replayed response from a re-executed one.
type harness struct {
	router http.Handler
	calls  *atomic.Int32
	status *atomic.Int32
	actor  httpserver.Actor
}

func newHarness(t *testing.T, db *postgres.DB, orgID uuid.UUID) *harness {
	t.Helper()

	h := &harness{
		calls:  &atomic.Int32{},
		status: &atomic.Int32{},
		actor:  httpserver.Actor{OrganizationID: orgID, Wallet: "0xabc", Role: httpserver.RoleOwner},
	}
	h.status.Store(http.StatusCreated)

	middleware := idempotency.NewMiddleware(db, func() time.Time { return testNow })

	h.router = httpserver.NewRouter(httpserver.Dependencies{
		Routes: func(r chi.Router) {
			r.Use(httpserver.Authenticate(httpserver.ResolverFunc(func(*http.Request) (httpserver.Actor, error) {
				return h.actor, nil
			})))
			r.Use(middleware.Handler)

			r.Post("/invoices", func(w http.ResponseWriter, req *http.Request) {
				count := h.calls.Add(1)
				status := int(h.status.Load())
				if status >= http.StatusBadRequest {
					httpserver.WriteProblem(w, req, apperr.Conflictf("the handler refused attempt %d", count))
					return
				}
				httpserver.WriteJSON(w, req, status, map[string]any{"id": "invoice-1", "call": count})
			})
			r.Get("/invoices", func(w http.ResponseWriter, req *http.Request) {
				h.calls.Add(1)
				httpserver.WriteJSON(w, req, http.StatusOK, map[string]string{"status": "ok"})
			})
		},
	})
	return h
}

func (h *harness) post(t *testing.T, key, body string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(http.MethodPost, "/api/v1/invoices", strings.NewReader(body))
	if key != "" {
		req.Header.Set(idempotency.HeaderKey, key)
	}
	rec := httptest.NewRecorder()
	h.router.ServeHTTP(rec, req)
	return rec
}

// TestRetryReplaysTheFirstResponse is the guarantee the whole package exists for: a client
// that never saw the first response gets it back instead of a second effect.
func TestRetryReplaysTheFirstResponse(t *testing.T) {
	db := pgtest.New(t)
	h := newHarness(t, db, seedOrganization(t, db, 1))

	first := h.post(t, "key-1", `{"number":"INV-1"}`)
	require.Equal(t, http.StatusCreated, first.Code)
	assert.Empty(t, first.Header().Get(idempotency.HeaderReplayed))

	second := h.post(t, "key-1", `{"number":"INV-1"}`)
	require.Equal(t, http.StatusCreated, second.Code)
	assert.Equal(t, "true", second.Header().Get(idempotency.HeaderReplayed))
	assert.JSONEq(t, first.Body.String(), second.Body.String())

	assert.Equal(t, int32(1), h.calls.Load(), "the handler ran once, however often the client retried")
}

// TestKeyReuseWithADifferentBodyIsRefused catches the client bug the fingerprint exists
// for: the same key sent with different content must not be answered from the first call.
func TestKeyReuseWithADifferentBodyIsRefused(t *testing.T) {
	db := pgtest.New(t)
	h := newHarness(t, db, seedOrganization(t, db, 2))

	require.Equal(t, http.StatusCreated, h.post(t, "key-1", `{"number":"INV-1"}`).Code)

	rec := h.post(t, "key-1", `{"number":"INV-2"}`)
	assert.Equal(t, http.StatusConflict, rec.Code)
	assert.Contains(t, rec.Body.String(), "different request")
	assert.Equal(t, int32(1), h.calls.Load())
}

// TestFailedRequestsCanBeRetried keeps a rejected attempt from poisoning its key: the
// client fixes the cause and retries with the same key.
func TestFailedRequestsCanBeRetried(t *testing.T) {
	db := pgtest.New(t)
	h := newHarness(t, db, seedOrganization(t, db, 3))

	h.status.Store(http.StatusConflict)
	require.Equal(t, http.StatusConflict, h.post(t, "key-1", `{"number":"INV-1"}`).Code)

	h.status.Store(http.StatusCreated)
	rec := h.post(t, "key-1", `{"number":"INV-1"}`)
	assert.Equal(t, http.StatusCreated, rec.Code)
	assert.Equal(t, int32(2), h.calls.Load(), "a failure released the key")
}

func TestKeysAreScopedPerEndpointAndOrganization(t *testing.T) {
	db := pgtest.New(t)
	ctx := context.Background()
	store := idempotency.NewStore()

	orgA := seedOrganization(t, db, 4)
	orgB := seedOrganization(t, db, 5)

	record := func(org uuid.UUID, endpoint string) *idempotency.Record {
		return &idempotency.Record{
			Key: "shared-key", OrganizationID: org, Endpoint: endpoint,
			RequestHash: "hash", CreatedAt: testNow,
		}
	}

	for _, rec := range []*idempotency.Record{
		record(orgA, "POST /api/v1/invoices"),
		record(orgA, "POST /api/v1/auctions"),
		record(orgB, "POST /api/v1/invoices"),
	} {
		var existing *idempotency.Record
		require.NoError(t, db.InTx(ctx, func(q postgres.Querier) error {
			var err error
			existing, err = store.Reserve(ctx, q, rec)
			return err
		}))
		assert.Nilf(t, existing, "%s/%s should be a fresh claim", rec.OrganizationID, rec.Endpoint)
	}

	// The same triple again is the one that collides.
	var existing *idempotency.Record
	require.NoError(t, db.InTx(ctx, func(q postgres.Querier) error {
		var err error
		existing, err = store.Reserve(ctx, q, record(orgA, "POST /api/v1/invoices"))
		return err
	}))
	assert.NotNil(t, existing)
}

// TestInFlightRetryIsRefused covers the racing retry: a second attempt arriving before the
// first finished must not run in parallel.
func TestInFlightRetryIsRefused(t *testing.T) {
	db := pgtest.New(t)
	ctx := context.Background()
	store := idempotency.NewStore()
	orgID := seedOrganization(t, db, 6)

	claim := &idempotency.Record{
		Key: "key-1", OrganizationID: orgID, Endpoint: "POST /api/v1/invoices",
		RequestHash: idempotency.Fingerprint(http.MethodPost, "/api/v1/invoices", []byte(`{"number":"INV-1"}`)),
		CreatedAt:   testNow,
	}
	require.NoError(t, db.InTx(ctx, func(q postgres.Querier) error {
		_, err := store.Reserve(ctx, q, claim)
		return err
	}))

	h := newHarness(t, db, orgID)
	rec := h.post(t, "key-1", `{"number":"INV-1"}`)

	assert.Equal(t, http.StatusConflict, rec.Code)
	assert.Contains(t, rec.Body.String(), "still in progress")
	assert.Equal(t, int32(0), h.calls.Load(), "the handler never ran a second time in parallel")
}

func TestMissingOrMalformedKeyIsRejected(t *testing.T) {
	db := pgtest.New(t)
	h := newHarness(t, db, seedOrganization(t, db, 7))

	tests := []struct {
		name string
		key  string
	}{
		{name: "missing", key: ""},
		{name: "too long", key: strings.Repeat("k", idempotency.MaxKeyLen+1)},
		{name: "control characters", key: "key\nwith-newline"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := h.post(t, tc.key, `{"number":"INV-1"}`)
			assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
			assert.Contains(t, rec.Body.String(), "Idempotency-Key")
		})
	}
	assert.Equal(t, int32(0), h.calls.Load())
}

// TestReadsNeedNoKey keeps the requirement where it belongs: only writes can be performed
// twice by accident.
func TestReadsNeedNoKey(t *testing.T) {
	db := pgtest.New(t)
	h := newHarness(t, db, seedOrganization(t, db, 8))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/invoices", http.NoBody)
	rec := httptest.NewRecorder()
	h.router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, int32(1), h.calls.Load())
}

func TestAnonymousWritesAreRejected(t *testing.T) {
	db := pgtest.New(t)
	h := newHarness(t, db, seedOrganization(t, db, 9))
	h.actor = httpserver.Actor{}

	rec := h.post(t, "key-1", `{"number":"INV-1"}`)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Equal(t, int32(0), h.calls.Load())
}

func TestStoreCompleteAndGet(t *testing.T) {
	db := pgtest.New(t)
	ctx := context.Background()
	store := idempotency.NewStore()
	orgID := seedOrganization(t, db, 10)

	rec := &idempotency.Record{
		Key: "key-1", OrganizationID: orgID, Endpoint: "POST /api/v1/invoices",
		RequestHash: "hash", CreatedAt: testNow,
	}
	require.NoError(t, db.InTx(ctx, func(q postgres.Querier) error {
		_, err := store.Reserve(ctx, q, rec)
		return err
	}))

	stored, err := store.Get(ctx, db.Querier(), orgID, rec.Endpoint, rec.Key)
	require.NoError(t, err)
	assert.False(t, stored.IsComplete())

	require.NoError(t, db.InTx(ctx, func(q postgres.Querier) error {
		return store.Complete(ctx, q, rec, http.StatusCreated, []byte(`{"id":"invoice-1"}`), testNow.Add(time.Second))
	}))

	stored, err = store.Get(ctx, db.Querier(), orgID, rec.Endpoint, rec.Key)
	require.NoError(t, err)
	require.True(t, stored.IsComplete())
	assert.Equal(t, http.StatusCreated, stored.StatusCode)
	assert.JSONEq(t, `{"id":"invoice-1"}`, string(stored.ResponseBody))

	_, err = store.Get(ctx, db.Querier(), orgID, rec.Endpoint, "unknown-key")
	require.ErrorIs(t, err, apperr.ErrNotFound)
}

// TestNonJSONResponsesAreNotReplayed keeps the store honest: it can only replay what it can
// hold, so anything else re-runs rather than replaying a corrupted body.
func TestNonJSONResponsesAreNotReplayed(t *testing.T) {
	db := pgtest.New(t)
	ctx := context.Background()
	store := idempotency.NewStore()
	orgID := seedOrganization(t, db, 11)

	rec := &idempotency.Record{
		Key: "key-1", OrganizationID: orgID, Endpoint: "POST /api/v1/invoices",
		RequestHash: "hash", CreatedAt: testNow,
	}
	require.NoError(t, db.InTx(ctx, func(q postgres.Querier) error {
		_, err := store.Reserve(ctx, q, rec)
		return err
	}))
	require.NoError(t, db.InTx(ctx, func(q postgres.Querier) error {
		return store.Complete(ctx, q, rec, http.StatusOK, []byte("not json at all"), testNow)
	}))

	stored, err := store.Get(ctx, db.Querier(), orgID, rec.Endpoint, rec.Key)
	require.NoError(t, err)
	assert.Equal(t, "null", string(stored.ResponseBody))
}

func TestFingerprint(t *testing.T) {
	t.Parallel()

	base := idempotency.Fingerprint(http.MethodPost, "/api/v1/invoices", []byte(`{"a":1}`))

	assert.Equal(t, base, idempotency.Fingerprint(http.MethodPost, "/api/v1/invoices", []byte(`{"a":1}`)))
	assert.NotEqual(t, base, idempotency.Fingerprint(http.MethodPost, "/api/v1/invoices", []byte(`{"a":2}`)))
	assert.NotEqual(t, base, idempotency.Fingerprint(http.MethodPut, "/api/v1/invoices", []byte(`{"a":1}`)))
	assert.NotEqual(t, base, idempotency.Fingerprint(http.MethodPost, "/api/v1/auctions", []byte(`{"a":1}`)))
}

func TestValidate(t *testing.T) {
	t.Parallel()

	require.NoError(t, idempotency.Validate("01J8Z9-abcdef"))
	require.ErrorIs(t, idempotency.Validate(""), apperr.ErrValidation)
	require.ErrorIs(t, idempotency.Validate(strings.Repeat("k", idempotency.MaxKeyLen+1)), apperr.ErrValidation)
	require.ErrorIs(t, idempotency.Validate("key with space"), apperr.ErrValidation)
}
