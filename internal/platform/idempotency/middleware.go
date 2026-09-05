package idempotency

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/skimer2king/factorflow/internal/platform/httpserver"
	"github.com/skimer2king/factorflow/internal/platform/postgres"
)

// HeaderKey is the header carrying the client's idempotency key.
const HeaderKey = "Idempotency-Key"

// HeaderReplayed marks a response served from a stored record rather than freshly computed.
const HeaderReplayed = "Idempotency-Replayed"

// Middleware enforces idempotent writes.
type Middleware struct {
	db    *postgres.DB
	store *Store
	now   func() time.Time
}

// NewMiddleware wires the middleware. The clock is injected so tests control record
// timestamps.
func NewMiddleware(db *postgres.DB, now func() time.Time) *Middleware {
	if now == nil {
		now = time.Now
	}
	return &Middleware{db: db, store: NewStore(), now: now}
}

// Handler wraps write routes.
//
// The flow is: validate the key, fingerprint the body, claim the key, run the handler, and
// store the response. A retry that arrives after the first attempt finished replays the
// stored response; one that arrives while it is still running is refused rather than run
// twice in parallel.
func (m *Middleware) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isWrite(r.Method) {
			next.ServeHTTP(w, r)
			return
		}

		key := r.Header.Get(HeaderKey)
		if err := Validate(key); err != nil {
			httpserver.WriteProblem(w, r, err)
			return
		}

		actor := httpserver.ActorFrom(r.Context())
		if actor.IsZero() {
			httpserver.WriteProblem(w, r, httpserver.ErrUnauthorized)
			return
		}

		body, err := io.ReadAll(r.Body)
		if err != nil {
			httpserver.WriteProblem(w, r, httpserver.ErrMalformedRequest)
			return
		}
		_ = r.Body.Close()
		r.Body = io.NopCloser(bytes.NewReader(body))

		record := &Record{
			Key:            key,
			OrganizationID: actor.OrganizationID,
			Endpoint:       r.Method + " " + routeOf(r),
			RequestHash:    Fingerprint(r.Method, r.URL.Path, body),
			CreatedAt:      m.now(),
		}

		existing, err := m.reserve(r.Context(), record)
		if err != nil {
			httpserver.WriteProblem(w, r, err)
			return
		}
		if existing != nil {
			m.replay(w, r, record, existing)
			return
		}

		capture := &responseCapture{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(capture, r)

		m.finish(r.Context(), record, capture)
	})
}

func (m *Middleware) reserve(ctx context.Context, record *Record) (*Record, error) {
	var existing *Record
	err := m.db.InTx(ctx, func(q postgres.Querier) error {
		var err error
		existing, err = m.store.Reserve(ctx, q, record)
		return err
	})
	if err != nil {
		return nil, err
	}
	return existing, nil
}

// replay answers a repeated request from the stored record.
func (m *Middleware) replay(w http.ResponseWriter, r *http.Request, incoming, stored *Record) {
	if stored.RequestHash != incoming.RequestHash {
		httpserver.WriteProblem(w, r, ConflictKeyReused())
		return
	}
	if !stored.IsComplete() {
		httpserver.WriteProblem(w, r, ConflictInFlight())
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set(HeaderReplayed, "true")
	w.WriteHeader(stored.StatusCode)
	if len(stored.ResponseBody) > 0 && string(stored.ResponseBody) != "null" {
		_, _ = w.Write(stored.ResponseBody)
	}
}

// finish stores a successful response, or releases the claim so a failure can be retried.
func (m *Middleware) finish(ctx context.Context, record *Record, capture *responseCapture) {
	// A failed request keeps no record: the client is expected to fix it and try again with
	// the same key, and replaying a 500 forever would be worse than useless.
	if capture.status >= http.StatusBadRequest {
		_ = m.db.InTx(ctx, func(q postgres.Querier) error {
			return m.store.Release(ctx, q, record)
		})
		return
	}

	_ = m.db.InTx(ctx, func(q postgres.Querier) error {
		return m.store.Complete(ctx, q, record, capture.status, capture.body.Bytes(), m.now())
	})
}

// isWrite reports whether the method changes state.
func isWrite(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
}

// routeOf names the endpoint an idempotency key is scoped to.
//
// Middleware runs before the final route is matched, so the pattern chi can report at this
// point is often still a wildcard such as "/api/v1/*". Scoping every write under one
// wildcard would put unrelated endpoints in the same namespace, so the request path is used
// whenever the pattern has not resolved to a concrete route yet.
func routeOf(r *http.Request) string {
	pattern := httpserver.RoutePattern(r)
	if pattern == "" || strings.Contains(pattern, "*") {
		return r.URL.Path
	}
	return pattern
}

// responseCapture records the handler's response so it can be stored for replay.
type responseCapture struct {
	http.ResponseWriter
	status      int
	body        bytes.Buffer
	wroteHeader bool
}

func (c *responseCapture) WriteHeader(status int) {
	if c.wroteHeader {
		return
	}
	c.status = status
	c.wroteHeader = true
	c.ResponseWriter.WriteHeader(status)
}

func (c *responseCapture) Write(b []byte) (int, error) {
	if !c.wroteHeader {
		c.WriteHeader(http.StatusOK)
	}
	c.body.Write(b)
	return c.ResponseWriter.Write(b)
}
