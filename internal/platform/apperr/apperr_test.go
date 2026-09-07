package apperr_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoldFridge/factorflow/internal/platform/apperr"
)

func TestFieldErrorMatchesValidation(t *testing.T) {
	t.Parallel()

	err := apperr.Invalid("face", "must be greater than zero, got %s", "0.00")

	require.ErrorIs(t, err, apperr.ErrValidation)
	assert.Equal(t, "face: must be greater than zero, got 0.00", err.Error())
	assert.NotErrorIs(t, err, apperr.ErrConflict)

	var fe *apperr.FieldError
	require.ErrorAs(t, err, &fe)
	assert.Equal(t, "face", fe.Field)
}

func TestFieldErrorWithoutFieldName(t *testing.T) {
	t.Parallel()

	err := apperr.Invalid("", "request body is not valid JSON")
	assert.Equal(t, "request body is not valid JSON", err.Error())
}

func TestFieldsCollectsEveryViolation(t *testing.T) {
	t.Parallel()

	err := errors.Join(
		apperr.Invalid("debtor_ref", "must not be empty"),
		apperr.Invalid("number", "must not be empty"),
		apperr.Invalid("face", "must be greater than zero"),
	)

	fields := apperr.Fields(err)
	require.Len(t, fields, 3, "errors.As alone would stop at the first violation")
	assert.Equal(t, []string{"debtor_ref", "number", "face"}, []string{fields[0].Field, fields[1].Field, fields[2].Field})
}

func TestFieldsLooksThroughWrapping(t *testing.T) {
	t.Parallel()

	wrapped := fmt.Errorf("creating invoice: %w", errors.Join(
		apperr.Invalid("id", "must be a non-nil UUID"),
		fmt.Errorf("validating dates: %w", apperr.Invalid("due_at", "must be after issued_at")),
	))

	fields := apperr.Fields(wrapped)
	require.Len(t, fields, 2)
	assert.Equal(t, "id", fields[0].Field)
	assert.Equal(t, "due_at", fields[1].Field)
}

func TestFieldsOnNonValidationErrors(t *testing.T) {
	t.Parallel()

	assert.Empty(t, apperr.Fields(nil))
	assert.Empty(t, apperr.Fields(errors.New("boom")))
	assert.Empty(t, apperr.Fields(apperr.Conflictf("invoice %s is already settled", "abc")))
}

func TestErrorKinds(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		err   error
		is    error
		isNot error
		want  string
	}{
		{
			name: "conflict", err: apperr.Conflictf("invoice %s cannot move from %s to %s", "abc", "DRAFT", "SETTLED"),
			is: apperr.ErrConflict, isNot: apperr.ErrValidation,
			want: "conflict: invoice abc cannot move from DRAFT to SETTLED",
		},
		{
			name: "not found", err: apperr.NotFoundf("invoice %s", "abc"),
			is: apperr.ErrNotFound, isNot: apperr.ErrConflict,
			want: "not found: invoice abc",
		},
		{
			name: "forbidden", err: apperr.Forbiddenf("organization %s", "abc"),
			is: apperr.ErrForbidden, isNot: apperr.ErrNotFound,
			want: "forbidden: organization abc",
		},
		{
			name: "unavailable", err: apperr.Unavailablef("market snapshot older than %s", "15m"),
			is: apperr.ErrUnavailable, isNot: apperr.ErrValidation,
			want: "dependency unavailable: market snapshot older than 15m",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			require.ErrorIs(t, tc.err, tc.is)
			assert.NotErrorIs(t, tc.err, tc.isNot)
			assert.Equal(t, tc.want, tc.err.Error())
		})
	}
}

func TestKindPredicates(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		err       error
		predicate func(error) bool
	}{
		{name: "validation", err: apperr.Invalid("face", "must be positive"), predicate: apperr.IsValidation},
		{name: "conflict", err: apperr.Conflictf("stale version"), predicate: apperr.IsConflict},
		{name: "not found", err: apperr.NotFoundf("invoice"), predicate: apperr.IsNotFound},
		{name: "forbidden", err: apperr.Forbiddenf("another issuer"), predicate: apperr.IsForbidden},
		{name: "unavailable", err: apperr.Unavailablef("gateway"), predicate: apperr.IsUnavailable},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			assert.True(t, tc.predicate(tc.err))
			assert.False(t, tc.predicate(errors.New("something else")))
			assert.False(t, tc.predicate(nil))

			// A predicate must not match a different kind.
			for _, other := range tests {
				if other.name == tc.name {
					continue
				}
				assert.Falsef(t, tc.predicate(other.err), "%s matched a %s error", tc.name, other.name)
			}
		})
	}
}
