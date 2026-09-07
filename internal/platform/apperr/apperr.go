// Package apperr defines the small set of error kinds that cross module boundaries.
//
// Domain code returns these instead of transport-specific errors, so the HTTP layer can
// map a failure to an RFC 9457 problem document without knowing which module produced it,
// and so a module can react to another module's failure without string matching.
package apperr

import (
	"errors"
	"fmt"
)

// Error kinds. Callers match them with errors.Is.
var (
	// ErrValidation reports input that violates a documented rule. Maps to HTTP 422.
	ErrValidation = errors.New("validation failed")
	// ErrConflict reports a state or version conflict, including a failed lifecycle
	// transition and a stale If-Match version. Maps to HTTP 409.
	ErrConflict = errors.New("conflict")
	// ErrNotFound reports a missing entity. Maps to HTTP 404.
	ErrNotFound = errors.New("not found")
	// ErrForbidden reports an authenticated caller acting outside its scope. Maps to 403.
	ErrForbidden = errors.New("forbidden")
	// ErrUnavailable reports a dependency that could not be reached or returned data the
	// system refuses to use, such as a stale market snapshot. Maps to HTTP 503.
	ErrUnavailable = errors.New("dependency unavailable")
)

// FieldError names the input field a validation failure belongs to, so the API can report
// per-field violations instead of one opaque message.
type FieldError struct {
	Field  string
	Detail string
}

// Invalid builds a field-scoped validation error.
func Invalid(field, format string, args ...any) error {
	return &FieldError{Field: field, Detail: fmt.Sprintf(format, args...)}
}

// Error implements error.
func (e *FieldError) Error() string {
	if e.Field == "" {
		return e.Detail
	}
	return e.Field + ": " + e.Detail
}

// Unwrap makes every field error match ErrValidation.
func (e *FieldError) Unwrap() error { return ErrValidation }

// Fields collects the field violations inside err, including those joined with
// errors.Join, in the order they were reported. errors.As is not enough here: it stops at
// the first match, and a request usually violates more than one rule.
func Fields(err error) []*FieldError {
	var out []*FieldError
	var walk func(error)
	walk = func(e error) {
		if e == nil {
			return
		}
		if fe, ok := e.(*FieldError); ok {
			out = append(out, fe)
			return
		}
		switch node := e.(type) {
		case interface{ Unwrap() []error }:
			for _, sub := range node.Unwrap() {
				walk(sub)
			}
		case interface{ Unwrap() error }:
			walk(node.Unwrap())
		}
	}
	walk(err)
	return out
}

// Conflictf reports a state or version conflict.
func Conflictf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrConflict, fmt.Sprintf(format, args...))
}

// NotFoundf reports a missing entity.
func NotFoundf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrNotFound, fmt.Sprintf(format, args...))
}

// Forbiddenf reports a caller acting outside its scope.
func Forbiddenf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrForbidden, fmt.Sprintf(format, args...))
}

// Unavailablef reports an unusable dependency.
func Unavailablef(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrUnavailable, fmt.Sprintf(format, args...))
}

// The predicates below name a kind without every caller writing errors.Is. They exist for
// code that must branch on the kind, such as deciding whether a failure is worth recording
// against an entity: a rejected command changed nothing, a genuine failure did.

// IsValidation reports whether err is a validation failure.
func IsValidation(err error) bool { return errors.Is(err, ErrValidation) }

// IsConflict reports whether err is a state or version conflict.
func IsConflict(err error) bool { return errors.Is(err, ErrConflict) }

// IsNotFound reports whether err is a missing entity.
func IsNotFound(err error) bool { return errors.Is(err, ErrNotFound) }

// IsForbidden reports whether err is an out-of-scope caller.
func IsForbidden(err error) bool { return errors.Is(err, ErrForbidden) }

// IsUnavailable reports whether err is an unusable dependency.
func IsUnavailable(err error) bool { return errors.Is(err, ErrUnavailable) }
