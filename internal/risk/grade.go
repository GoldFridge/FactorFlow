package risk

import (
	"fmt"

	"github.com/GoldFridge/factorflow/internal/platform/apperr"
	"github.com/GoldFridge/factorflow/internal/platform/money"
)

// Grade is the published risk band of an assessed receivable. Investors bid against a
// maximum grade, so the mapping from PD to grade is part of the model's public contract.
//
//	A: PD below 2%      B: 2% to 5%      C: 5% to 10%      D: 10% to 20%      E: above 20%
type Grade string

// The risk grades, ordered from safest to riskiest.
const (
	GradeA Grade = "A"
	GradeB Grade = "B"
	GradeC Grade = "C"
	GradeD Grade = "D"
	GradeE Grade = "E"
)

// gradeBands are the upper PD bounds, exclusive, in order. The last band has no bound.
var gradeBands = []struct {
	grade Grade
	upper money.Rate
}{
	{GradeA, money.MustParseRate("0.02")},
	{GradeB, money.MustParseRate("0.05")},
	{GradeC, money.MustParseRate("0.10")},
	{GradeD, money.MustParseRate("0.20")},
	{GradeE, money.Rate{}}, // unbounded
}

// gradeRanks orders grades for comparison against an investor's maximum.
var gradeRanks = map[Grade]int{GradeA: 0, GradeB: 1, GradeC: 2, GradeD: 3, GradeE: 4}

// GradeFromPD maps a probability of default to its band. Bounds are exclusive at the top,
// so a PD of exactly 2% is a B, not an A.
func GradeFromPD(pd money.Rate) Grade {
	for _, band := range gradeBands[:len(gradeBands)-1] {
		if pd.Cmp(band.upper) < 0 {
			return band.grade
		}
	}
	return GradeE
}

// ParseGrade validates a grade read from storage or a bid.
func ParseGrade(s string) (Grade, error) {
	g := Grade(s)
	if !g.IsValid() {
		return "", fmt.Errorf("%w: unknown risk grade %q", apperr.ErrValidation, s)
	}
	return g, nil
}

// IsValid reports whether the grade is one of A..E.
func (g Grade) IsValid() bool {
	_, ok := gradeRanks[g]
	return ok
}

// Rank returns the ordering position, 0 for the safest grade. It panics for an invalid
// grade; parse before ranking.
func (g Grade) Rank() int {
	rank, ok := gradeRanks[g]
	if !ok {
		panic(fmt.Sprintf("risk: rank requested for invalid grade %q", string(g)))
	}
	return rank
}

// AtMost reports whether g is no riskier than limit, the check an investor's maximum grade
// constraint performs.
func (g Grade) AtMost(limit Grade) bool { return g.Rank() <= limit.Rank() }

// String returns the wire representation.
func (g Grade) String() string { return string(g) }
