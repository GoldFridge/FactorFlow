package httpserver

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// contextKey is the private key type for values this package puts on a request context.
type contextKey int

const (
	traceIDKey contextKey = iota
)

// TraceIDHeader carries the trace identifier in and out of the service.
const TraceIDHeader = "X-Trace-Id"

// TraceID assigns each request an identifier and echoes it back.
//
// The specification asks for one trace id across API, outbox, confidential workflow and
// chain calls. This is where it starts: an inbound id is honoured so a caller can correlate
// its own logs, and a fresh one is minted otherwise.
func TraceID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		traceID := sanitizeTraceID(r.Header.Get(TraceIDHeader))
		if traceID == "" {
			traceID = newTraceID()
		}

		ctx := context.WithValue(r.Context(), traceIDKey, traceID)
		w.Header().Set(TraceIDHeader, traceID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// TraceIDFrom returns the trace id carried by a context, or the empty string.
func TraceIDFrom(ctx context.Context) string {
	traceID, _ := ctx.Value(traceIDKey).(string)
	return traceID
}

// ContextWithTraceID attaches a trace id, so a worker can continue a trace that started in
// an HTTP request.
func ContextWithTraceID(ctx context.Context, traceID string) context.Context {
	return context.WithValue(ctx, traceIDKey, sanitizeTraceID(traceID))
}

func newTraceID() string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// A trace id is diagnostic, never a security boundary; a timestamp is a usable
		// fallback and is better than failing the request.
		return "t" + time.Now().UTC().Format("20060102150405.000000000")
	}
	return hex.EncodeToString(buf[:])
}

// sanitizeTraceID keeps an inbound id only if it is short and printable, so a caller
// cannot inject newlines into the logs or an unbounded string into every log line.
func sanitizeTraceID(value string) string {
	const maxLen = 64

	value = strings.TrimSpace(value)
	if len(value) > maxLen {
		return ""
	}
	for _, r := range value {
		if r < '!' || r > '~' {
			return ""
		}
	}
	return value
}

// Recover turns a panicking handler into a 500 problem document.
//
// Without it a panic takes down the connection with no response and no log line naming the
// request, which on a demo server is the difference between a bug report and a mystery.
func Recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			recovered := recover()
			if recovered == nil {
				return
			}
			// A panic after the response started cannot be turned into a problem document;
			// log it and let the truncated response speak for itself.
			if wrapped, ok := w.(*statusRecorder); ok && wrapped.wroteHeader {
				slog.ErrorContext(r.Context(), "panic after response started",
					slog.Any("panic", recovered),
					slog.String("trace_id", TraceIDFrom(r.Context())))
				return
			}

			slog.ErrorContext(r.Context(), "panic recovered",
				slog.Any("panic", recovered),
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.String("trace_id", TraceIDFrom(r.Context())))

			WriteProblem(w, r, errUnexpected)
		}()

		next.ServeHTTP(w, r)
	})
}

// errUnexpected is deliberately unmapped, so it renders as a 500 with a generic detail.
var errUnexpected = &unexpectedError{}

type unexpectedError struct{}

func (*unexpectedError) Error() string { return "unexpected error" }

// statusRecorder captures the status code for logging.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	bytes       int
	wroteHeader bool
}

func (w *statusRecorder) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.status = status
	w.wroteHeader = true
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusRecorder) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += n
	return n, err
}

// LogRequests writes one structured line per request.
//
// It logs the route, not the raw path: a path can carry an invoice id, and the privacy rule
// keeps identifiers of private documents out of the logs. Query strings are never logged
// for the same reason.
func LogRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

		next.ServeHTTP(recorder, r)

		slog.InfoContext(r.Context(), "request",
			slog.String("method", r.Method),
			slog.String("route", routePattern(r)),
			slog.Int("status", recorder.status),
			slog.Int("bytes", recorder.bytes),
			slog.Duration("duration", time.Since(started).Round(time.Millisecond)),
			slog.String("trace_id", TraceIDFrom(r.Context())))
	})
}

// SecurityHeaders sets the response headers the specification requires.
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := w.Header()
		header.Set("X-Content-Type-Options", "nosniff")
		header.Set("X-Frame-Options", "DENY")
		header.Set("Referrer-Policy", "no-referrer")
		// The API serves JSON only, so the strictest policy that still works is one that
		// forbids everything.
		header.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
		header.Set("Cache-Control", "no-store")

		next.ServeHTTP(w, r)
	})
}

// MaxBodyBytes rejects a request body larger than the limit before a handler reads it.
func MaxBodyBytes(limit int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.Body = http.MaxBytesReader(w, r.Body, limit)
			next.ServeHTTP(w, r)
		})
	}
}
