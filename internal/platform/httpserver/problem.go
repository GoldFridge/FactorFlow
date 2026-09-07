// Package httpserver holds the transport plumbing shared by every module's handlers: the
// router, middleware, and the error and JSON conventions the API promises.
//
// Handlers in a domain module map requests to service calls; this package decides what an
// error looks like on the wire, what a request carries with it, and what a response always
// includes. Keeping that here is what makes the API consistent across ten handlers written
// at different times.
package httpserver

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/GoldFridge/factorflow/internal/platform/apperr"
)

// ProblemContentType is the media type RFC 9457 defines for problem documents.
const ProblemContentType = "application/problem+json"

// problemBaseURI namespaces the machine-readable error codes.
const problemBaseURI = "https://factorflow.dev/problems/"

// Problem is an RFC 9457 problem document.
//
// Every failure the API returns takes this shape, so a client parses one error format
// rather than guessing per endpoint. The detail is safe to show a user: it never carries
// document content, a signed URL, or an internal error string.
type Problem struct {
	// Type is a URI identifying the problem kind.
	Type string `json:"type"`
	// Title is a short, stable summary of the problem kind.
	Title string `json:"title"`
	// Status repeats the HTTP status code, so a logged document is self-contained.
	Status int `json:"status"`
	// Detail explains this occurrence.
	Detail string `json:"detail,omitempty"`
	// Instance identifies the request that failed.
	Instance string `json:"instance,omitempty"`
	// Code is the machine-readable error code a client switches on.
	Code string `json:"code"`
	// TraceID ties the response to the server-side trace, so a user can quote it in a bug
	// report and an operator can find the request.
	TraceID string `json:"trace_id,omitempty"`
	// Violations lists per-field validation failures.
	Violations []Violation `json:"violations,omitempty"`
}

// Violation is one field-level validation failure.
type Violation struct {
	Field  string `json:"field"`
	Detail string `json:"detail"`
}

// Error codes returned by the API.
const (
	CodeValidation   = "validation_failed"
	CodeConflict     = "conflict"
	CodeNotFound     = "not_found"
	CodeForbidden    = "forbidden"
	CodeUnauthorized = "unauthorized"
	CodeUnavailable  = "dependency_unavailable"
	CodeInternal     = "internal_error"
	CodeMalformed    = "malformed_request"
)

// WriteProblem renders an error as a problem document.
//
// The mapping is deliberately narrow: an error kind the application defined maps to its
// status, and anything else becomes a 500 with a generic detail. An unrecognised error is
// a bug, and leaking its message would leak whatever the bug touched.
func WriteProblem(w http.ResponseWriter, r *http.Request, err error) {
	problem := problemFor(err, r)

	if problem.Status >= http.StatusInternalServerError {
		slog.ErrorContext(r.Context(), "request failed",
			slog.String("code", problem.Code),
			slog.String("trace_id", problem.TraceID),
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.String("error", err.Error()))
	} else {
		slog.InfoContext(r.Context(), "request rejected",
			slog.String("code", problem.Code),
			slog.String("trace_id", problem.TraceID),
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("status", problem.Status))
	}

	w.Header().Set("Content-Type", ProblemContentType)
	w.WriteHeader(problem.Status)
	if encodeErr := json.NewEncoder(w).Encode(problem); encodeErr != nil {
		slog.ErrorContext(r.Context(), "writing problem document", slog.String("error", encodeErr.Error()))
	}
}

func problemFor(err error, r *http.Request) Problem {
	problem := Problem{
		Instance: r.URL.Path,
		TraceID:  TraceIDFrom(r.Context()),
	}

	switch {
	case errors.Is(err, apperr.ErrValidation):
		problem.Status = http.StatusUnprocessableEntity
		problem.Code = CodeValidation
		problem.Title = "The request is not valid"
		problem.Detail = err.Error()
		for _, field := range apperr.Fields(err) {
			problem.Violations = append(problem.Violations, Violation{Field: field.Field, Detail: field.Detail})
		}
	case errors.Is(err, apperr.ErrConflict):
		problem.Status = http.StatusConflict
		problem.Code = CodeConflict
		problem.Title = "The request conflicts with the current state"
		problem.Detail = err.Error()
	case errors.Is(err, apperr.ErrNotFound):
		problem.Status = http.StatusNotFound
		problem.Code = CodeNotFound
		problem.Title = "The resource does not exist"
		problem.Detail = err.Error()
	case errors.Is(err, apperr.ErrForbidden):
		problem.Status = http.StatusForbidden
		problem.Code = CodeForbidden
		problem.Title = "The caller may not perform this action"
		problem.Detail = err.Error()
	case errors.Is(err, apperr.ErrUnavailable):
		problem.Status = http.StatusServiceUnavailable
		problem.Code = CodeUnavailable
		problem.Title = "A dependency is unavailable"
		problem.Detail = err.Error()
	case errors.Is(err, ErrUnauthorized):
		problem.Status = http.StatusUnauthorized
		problem.Code = CodeUnauthorized
		problem.Title = "Authentication is required"
		problem.Detail = err.Error()
	case errors.Is(err, ErrMalformedRequest):
		problem.Status = http.StatusBadRequest
		problem.Code = CodeMalformed
		problem.Title = "The request could not be read"
		problem.Detail = err.Error()
	default:
		// Nothing from the error reaches the client: an unmapped error is a bug, and its
		// message may quote whatever the bug was handling.
		problem.Status = http.StatusInternalServerError
		problem.Code = CodeInternal
		problem.Title = "The request could not be completed"
		problem.Detail = "An unexpected error occurred. Quote the trace id when reporting it."
	}

	problem.Type = problemBaseURI + problem.Code
	return problem
}

// Sentinel errors the transport layer itself raises.
var (
	// ErrUnauthorized reports a missing or invalid session.
	ErrUnauthorized = errors.New("authentication required")
	// ErrMalformedRequest reports a body that could not be read at all, as opposed to one
	// that was read and failed validation.
	ErrMalformedRequest = errors.New("malformed request")
)

// errForbiddenOperator is returned when a non-operator calls an operator-only route.
var errForbiddenOperator = fmt.Errorf("%w: this action requires a platform operator", apperr.ErrForbidden)
