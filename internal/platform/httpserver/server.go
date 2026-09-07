package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/GoldFridge/factorflow/internal/platform/apperr"
)

// Limits the transport enforces before a handler runs.
const (
	// MaxRequestBytes bounds a JSON request body. Encrypted documents go to object
	// storage, never through the API, so no legitimate request is large.
	MaxRequestBytes int64 = 1 << 20
	// ReadHeaderTimeout bounds how long a client may take to send its headers, which is
	// what closes the trivial slow-loris hole in a default net/http server.
	ReadHeaderTimeout = 10 * time.Second
	// ShutdownTimeout bounds graceful shutdown before in-flight requests are dropped.
	ShutdownTimeout = 15 * time.Second
)

// APIPrefix is the versioned base path of the API.
const APIPrefix = "/api/v1"

// Dependencies are what the router needs from the rest of the application.
type Dependencies struct {
	// Ready reports whether the service can serve traffic, usually a database ping. A nil
	// check means "always ready", which is only correct in tests.
	Ready func(ctx context.Context) error
	// Version is reported by the health endpoints so a demo can prove which build is live.
	Version string
	// Routes registers the domain endpoints under the API prefix. Keeping this a callback
	// is what stops this package from importing every domain module.
	Routes func(r chi.Router)
}

// NewRouter builds the HTTP router with the middleware every request passes through.
func NewRouter(deps Dependencies) http.Handler {
	router := chi.NewRouter()

	// Order matters: a trace id must exist before anything logs, the recovery must wrap
	// the handlers rather than the logging, and the body limit must apply before a handler
	// reads anything.
	router.Use(TraceID)
	router.Use(SecurityHeaders)
	router.Use(LogRequests)
	router.Use(Recover)
	router.Use(MaxBodyBytes(MaxRequestBytes))

	router.Get("/healthz", healthHandler(deps.Version))
	router.Get("/readyz", readyHandler(deps))

	if deps.Routes != nil {
		router.Route(APIPrefix, deps.Routes)
	}

	router.NotFound(func(w http.ResponseWriter, r *http.Request) {
		WriteProblem(w, r, apperr.NotFoundf("no route for %s %s", r.Method, r.URL.Path))
	})
	router.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		WriteProblem(w, r, apperr.NotFoundf("method %s is not allowed here", r.Method))
	})

	return router
}

// NewServer wraps a handler in an http.Server with the timeouts a public endpoint needs.
func NewServer(addr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: ReadHeaderTimeout,
		// No write timeout: a chain call or a confidential workflow can legitimately keep a
		// request open longer than any fixed limit would allow, and the handler's own
		// context deadline is the right place to bound it.
		IdleTimeout: 90 * time.Second,
	}
}

// healthResponse is what the liveness and readiness endpoints return.
type healthResponse struct {
	Status  string `json:"status"`
	Version string `json:"version,omitempty"`
}

// healthHandler answers liveness: the process is up and serving.
func healthHandler(version string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		WriteJSON(w, r, http.StatusOK, healthResponse{Status: "ok", Version: version})
	}
}

// readyHandler answers readiness: the dependencies this service cannot work without are
// reachable.
//
// Only the database counts. An outage at a market data provider or a chain node must not
// take the service out of rotation: the specification asks for a degraded read-only mode,
// not a dead server.
func readyHandler(deps Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Ready != nil {
			ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
			defer cancel()

			if err := deps.Ready(ctx); err != nil {
				WriteProblem(w, r, apperr.Unavailablef("not ready: %v", err))
				return
			}
		}
		WriteJSON(w, r, http.StatusOK, healthResponse{Status: "ready", Version: deps.Version})
	}
}

// WriteJSON renders a successful response.
func WriteJSON(w http.ResponseWriter, r *http.Request, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)

	if payload == nil || status == http.StatusNoContent {
		return
	}
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		// The status line is already sent, so the response cannot become a problem
		// document; the log is the only place left to record it.
		logEncodeFailure(r, err)
	}
}

// DecodeJSON reads a JSON request body into dst.
//
// Unknown fields are rejected rather than ignored: a client sending "max_yield" when the
// field is "min_yield" should be told, not silently given a zero limit. Trailing content
// is rejected for the same reason.
func DecodeJSON(r *http.Request, dst any) error {
	if r.Body == nil {
		return fmt.Errorf("%w: the request has no body", ErrMalformedRequest)
	}

	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(dst); err != nil {
		var maxBytes *http.MaxBytesError
		if errors.As(err, &maxBytes) {
			return fmt.Errorf("%w: the request body is larger than %d bytes", ErrMalformedRequest, maxBytes.Limit)
		}
		var syntaxErr *json.SyntaxError
		if errors.As(err, &syntaxErr) {
			return fmt.Errorf("%w: invalid JSON at byte %d", ErrMalformedRequest, syntaxErr.Offset)
		}
		var typeErr *json.UnmarshalTypeError
		if errors.As(err, &typeErr) {
			return apperr.Invalid(typeErr.Field, "must be a %s", typeErr.Type.String())
		}
		if errors.Is(err, io.EOF) {
			return fmt.Errorf("%w: the request body is empty", ErrMalformedRequest)
		}
		return fmt.Errorf("%w: %v", ErrMalformedRequest, err)
	}

	if decoder.More() {
		return fmt.Errorf("%w: the request body has trailing content", ErrMalformedRequest)
	}
	return nil
}

// routePattern returns the matched route, falling back to the path when chi has not
// matched one, as on a 404.
func routePattern(r *http.Request) string {
	if rctx := chi.RouteContext(r.Context()); rctx != nil {
		if pattern := rctx.RoutePattern(); pattern != "" {
			return pattern
		}
	}
	return r.URL.Path
}

// RoutePattern returns the matched route pattern for a request, such as
// "/api/v1/invoices/{invoiceID}".
//
// Middleware outside this package uses it instead of the raw path: a path carries entity
// identifiers, and both the logs and the idempotency namespace want the route, not the
// particular invoice.
func RoutePattern(r *http.Request) string { return routePattern(r) }
