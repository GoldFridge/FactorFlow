package httpserver

import (
	"log/slog"
	"net/http"
)

// logEncodeFailure records a response that could not be serialized after the status line
// was already sent.
func logEncodeFailure(r *http.Request, err error) {
	slog.ErrorContext(r.Context(), "encoding response",
		slog.String("route", routePattern(r)),
		slog.String("trace_id", TraceIDFrom(r.Context())),
		slog.String("error", err.Error()))
}
