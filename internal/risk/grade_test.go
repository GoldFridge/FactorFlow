package risk_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/skimer2king/factorflow/internal/platform/apperr"
	"github.com/skimer2king/factorflow/internal/platform/money"
	"github.com/skimer2king/factorflow/internal/risk"
)

func TestGradeFromPD(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		pd   string
		want risk.Grade
	}{
		{name: "floor", pd: "0.001", want: risk.GradeA},
		{name: "just under 2 percent", pd: "0.019999", want: risk.GradeA},
		{name: "exactly 2 percent is B", pd: "0.02", want: risk.GradeB},
		{name: "mid B", pd: "0.031", want: risk.GradeB},
		{name: "exactly 5 percent is C", pd: "0.05", want: risk.GradeC},
		{name: "mid C", pd: "0.087", want: risk.GradeC},
		{name: "exactly 10 percent is D", pd: "0.10", want: risk.GradeD},
		{name: "mid D", pd: "0.155", want: risk.GradeD},
		{name: "exactly 20 percent is E", pd: "0.20", want: risk.GradeE},
		{name: "ceiling", pd: "0.80", want: risk.GradeE},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, risk.GradeFromPD(money.MustParseRate(tc.pd)))
		})
	}
}

func TestParseGrade(t *testing.T) {
	t.Parallel()

	got, err := risk.ParseGrade("C")
	require.NoError(t, err)
	assert.Equal(t, risk.GradeC, got)
	assert.Equal(t, "C", got.String())

	_, err = risk.ParseGrade("c")
	require.ErrorIs(t, err, apperr.ErrValidation)

	_, err = risk.ParseGrade("AAA")
	require.ErrorIs(t, err, apperr.ErrValidation)
}

func TestGradeOrdering(t *testing.T) {
	t.Parallel()

	// An investor bidding "at most C" accepts A, B and C, and refuses D and E.
	assert.True(t, risk.GradeA.AtMost(risk.GradeC))
	assert.True(t, risk.GradeB.AtMost(risk.GradeC))
	assert.True(t, risk.GradeC.AtMost(risk.GradeC))
	assert.False(t, risk.GradeD.AtMost(risk.GradeC))
	assert.False(t, risk.GradeE.AtMost(risk.GradeC))

	assert.Equal(t, 0, risk.GradeA.Rank())
	assert.Equal(t, 4, risk.GradeE.Rank())
	assert.Panics(t, func() { risk.Grade("Z").Rank() })
	assert.False(t, risk.Grade("Z").IsValid())
}
