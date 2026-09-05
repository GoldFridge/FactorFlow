package httpserver_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/skimer2king/factorflow/internal/platform/apperr"
	"github.com/skimer2king/factorflow/internal/platform/httpserver"
)

// newTestRouter builds a router with a handful of routes that fail in each documented way.
func newTestRouter(t *testing.T, ready func(context.Context) error) http.Handler {
	t.Helper()

	return httpserver.NewRouter(httpserver.Dependencies{
		Version: "test",
		Ready:   ready,
		Routes: func(r chi.Router) {
			r.Get("/ok", func(w http.ResponseWriter, r *http.Request) {
				httpserver.WriteJSON(w, r, http.StatusOK, map[string]string{"hello": "world"})
			})
			r.Get("/fail/{kind}", func(w http.ResponseWriter, r *http.Request) {
				httpserver.WriteProblem(w, r, failureFor(chi.URLParam(r, "kind")))
			})
			r.Post("/echo", func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Name  string `json:"name"`
					Count int    `json:"count"`
				}
				if err := httpserver.DecodeJSON(r, &body); err != nil {
					httpserver.WriteProblem(w, r, err)
					return
				}
				httpserver.WriteJSON(w, r, http.StatusCreated, body)
			})
			r.Get("/panic", func(http.ResponseWriter, *http.Request) {
				panic("boom")
			})
		},
	})
}

func failureFor(kind string) error {
	switch kind {
	case "validation":
		return errors.Join(
			apperr.Invalid("face", "must be greater than zero"),
			apperr.Invalid("due_at", "must be after issued_at"),
		)
	case "conflict":
		return apperr.Conflictf("invoice cannot move from DRAFT to SETTLED")
	case "notfound":
		return apperr.NotFoundf("invoice abc")
	case "forbidden":
		return apperr.Forbiddenf("organization abc")
	case "unavailable":
		return apperr.Unavailablef("DATA_STALE: market snapshot is 20m old")
	case "unauthorized":
		return httpserver.ErrUnauthorized
	case "malformed":
		return httpserver.ErrMalformedRequest
	default:
		return errors.New("secret internal detail that must not leak")
	}
}

func do(t *testing.T, router http.Handler, method, path string, body string) *httptest.ResponseRecorder {
	t.Helper()

	var reader io.Reader = http.NoBody
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func decodeProblem(t *testing.T, rec *httptest.ResponseRecorder) httpserver.Problem {
	t.Helper()

	var problem httpserver.Problem
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &problem))
	return problem
}

func TestHealthAndReadiness(t *testing.T) {
	t.Parallel()

	router := newTestRouter(t, nil)

	rec := do(t, router, http.MethodGet, "/healthz", "")
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"status":"ok"`)
	assert.Contains(t, rec.Body.String(), `"version":"test"`)

	rec = do(t, router, http.MethodGet, "/readyz", "")
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"status":"ready"`)
}

// TestReadinessFailsWhenTheDatabaseIsDown is the rule the deployment depends on: readiness
// goes false only for a dependency the service cannot work without.
func TestReadinessFailsWhenTheDatabaseIsDown(t *testing.T) {
	t.Parallel()

	router := newTestRouter(t, func(context.Context) error {
		return errors.New("connection refused")
	})

	rec := do(t, router, http.MethodGet, "/readyz", "")
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Equal(t, httpserver.ProblemContentType, rec.Header().Get("Content-Type"))

	problem := decodeProblem(t, rec)
	assert.Equal(t, httpserver.CodeUnavailable, problem.Code)

	// Liveness stays up: the process is fine, its dependency is not.
	rec = do(t, router, http.MethodGet, "/healthz", "")
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestErrorKindsMapToStatuses(t *testing.T) {
	t.Parallel()

	router := newTestRouter(t, nil)

	tests := []struct {
		kind       string
		wantStatus int
		wantCode   string
	}{
		{"validation", http.StatusUnprocessableEntity, httpserver.CodeValidation},
		{"conflict", http.StatusConflict, httpserver.CodeConflict},
		{"notfound", http.StatusNotFound, httpserver.CodeNotFound},
		{"forbidden", http.StatusForbidden, httpserver.CodeForbidden},
		{"unavailable", http.StatusServiceUnavailable, httpserver.CodeUnavailable},
		{"unauthorized", http.StatusUnauthorized, httpserver.CodeUnauthorized},
		{"malformed", http.StatusBadRequest, httpserver.CodeMalformed},
		{"unknown", http.StatusInternalServerError, httpserver.CodeInternal},
	}

	for _, tc := range tests {
		t.Run(tc.kind, func(t *testing.T) {
			t.Parallel()

			rec := do(t, router, http.MethodGet, "/api/v1/fail/"+tc.kind, "")
			require.Equal(t, tc.wantStatus, rec.Code)
			assert.Equal(t, httpserver.ProblemContentType, rec.Header().Get("Content-Type"))

			problem := decodeProblem(t, rec)
			assert.Equal(t, tc.wantCode, problem.Code)
			assert.Equal(t, tc.wantStatus, problem.Status)
			assert.NotEmpty(t, problem.Title)
			assert.NotEmpty(t, problem.Type)
			assert.NotEmpty(t, problem.TraceID, "every problem carries the trace id to quote")
			assert.Equal(t, "/api/v1/fail/"+tc.kind, problem.Instance)
		})
	}
}

// TestInternalErrorsDoNotLeak is a privacy requirement, not a style preference: an unmapped
// error may quote whatever the failing code was handling.
func TestInternalErrorsDoNotLeak(t *testing.T) {
	t.Parallel()

	rec := do(t, newTestRouter(t, nil), http.MethodGet, "/api/v1/fail/unknown", "")
	require.Equal(t, http.StatusInternalServerError, rec.Code)

	assert.NotContains(t, rec.Body.String(), "secret internal detail")
	problem := decodeProblem(t, rec)
	assert.Contains(t, problem.Detail, "trace id")
}

func TestValidationProblemListsEveryField(t *testing.T) {
	t.Parallel()

	rec := do(t, newTestRouter(t, nil), http.MethodGet, "/api/v1/fail/validation", "")
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code)

	problem := decodeProblem(t, rec)
	require.Len(t, problem.Violations, 2, "a client fixes one form, not one field per round trip")
	assert.Equal(t, "face", problem.Violations[0].Field)
	assert.Equal(t, "due_at", problem.Violations[1].Field)
}

func TestDecodeJSON(t *testing.T) {
	t.Parallel()

	router := newTestRouter(t, nil)

	rec := do(t, router, http.MethodPost, "/api/v1/echo", `{"name":"acme","count":3}`)
	require.Equal(t, http.StatusCreated, rec.Code)
	assert.JSONEq(t, `{"name":"acme","count":3}`, rec.Body.String())

	tests := []struct {
		name       string
		body       string
		wantStatus int
		wantCode   string
	}{
		{name: "empty body", body: "", wantStatus: http.StatusBadRequest, wantCode: httpserver.CodeMalformed},
		{name: "broken json", body: `{"name":`, wantStatus: http.StatusBadRequest, wantCode: httpserver.CodeMalformed},
		{name: "trailing content", body: `{"name":"a"}{"name":"b"}`, wantStatus: http.StatusBadRequest, wantCode: httpserver.CodeMalformed},
		{name: "unknown field", body: `{"name":"a","nickname":"b"}`, wantStatus: http.StatusBadRequest, wantCode: httpserver.CodeMalformed},
		{name: "wrong type", body: `{"count":"three"}`, wantStatus: http.StatusUnprocessableEntity, wantCode: httpserver.CodeValidation},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			rec := do(t, router, http.MethodPost, "/api/v1/echo", tc.body)
			require.Equal(t, tc.wantStatus, rec.Code)
			assert.Equal(t, tc.wantCode, decodeProblem(t, rec).Code)
		})
	}
}

func TestOversizedBodyIsRejected(t *testing.T) {
	t.Parallel()

	huge := `{"name":"` + strings.Repeat("x", int(httpserver.MaxRequestBytes)+10) + `"}`
	rec := do(t, newTestRouter(t, nil), http.MethodPost, "/api/v1/echo", huge)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, httpserver.CodeMalformed, decodeProblem(t, rec).Code)
}

func TestPanicBecomesAProblemDocument(t *testing.T) {
	t.Parallel()

	rec := do(t, newTestRouter(t, nil), http.MethodGet, "/api/v1/panic", "")

	require.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.NotContains(t, rec.Body.String(), "boom", "a panic message never reaches the client")
	assert.Equal(t, httpserver.CodeInternal, decodeProblem(t, rec).Code)
}

func TestTraceIDIsAssignedAndEchoed(t *testing.T) {
	t.Parallel()

	router := newTestRouter(t, nil)

	rec := do(t, router, http.MethodGet, "/api/v1/ok", "")
	assigned := rec.Header().Get(httpserver.TraceIDHeader)
	assert.NotEmpty(t, assigned)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/ok", http.NoBody)
	req.Header.Set(httpserver.TraceIDHeader, "client-supplied-id")
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	assert.Equal(t, "client-supplied-id", rec.Header().Get(httpserver.TraceIDHeader),
		"a caller's own id is honoured so both sides can correlate")
}

// TestHostileTraceIDsAreReplaced stops a caller from injecting newlines into the log or an
// unbounded string into every log line.
func TestHostileTraceIDsAreReplaced(t *testing.T) {
	t.Parallel()

	router := newTestRouter(t, nil)

	for _, hostile := range []string{
		"line\nbreak",
		strings.Repeat("x", 200),
		"tab\there",
	} {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/ok", http.NoBody)
		req.Header.Set(httpserver.TraceIDHeader, hostile)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)

		got := rec.Header().Get(httpserver.TraceIDHeader)
		assert.NotEqual(t, hostile, got)
		assert.NotEmpty(t, got, "a rejected id is replaced, not dropped")
	}
}

func TestSecurityHeaders(t *testing.T) {
	t.Parallel()

	rec := do(t, newTestRouter(t, nil), http.MethodGet, "/api/v1/ok", "")

	assert.Equal(t, "nosniff", rec.Header().Get("X-Content-Type-Options"))
	assert.Equal(t, "DENY", rec.Header().Get("X-Frame-Options"))
	assert.Equal(t, "no-referrer", rec.Header().Get("Referrer-Policy"))
	assert.Contains(t, rec.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'")
	assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
}

func TestUnknownRoutesAndMethods(t *testing.T) {
	t.Parallel()

	router := newTestRouter(t, nil)

	rec := do(t, router, http.MethodGet, "/api/v1/nope", "")
	require.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, httpserver.CodeNotFound, decodeProblem(t, rec).Code)

	rec = do(t, router, http.MethodDelete, "/api/v1/ok", "")
	require.Equal(t, http.StatusNotFound, rec.Code)
}

func TestContextWithTraceID(t *testing.T) {
	t.Parallel()

	ctx := httpserver.ContextWithTraceID(context.Background(), "worker-trace")
	assert.Equal(t, "worker-trace", httpserver.TraceIDFrom(ctx))

	assert.Empty(t, httpserver.TraceIDFrom(context.Background()))
	assert.Empty(t, httpserver.TraceIDFrom(httpserver.ContextWithTraceID(context.Background(), "bad\nid")))
}

func TestNewServerTimeouts(t *testing.T) {
	t.Parallel()

	server := httpserver.NewServer(":0", newTestRouter(t, nil))

	assert.Equal(t, ":0", server.Addr)
	assert.Equal(t, httpserver.ReadHeaderTimeout, server.ReadHeaderTimeout,
		"a server with no header timeout is trivially held open")
	assert.NotNil(t, server.Handler)
}
