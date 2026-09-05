package money

import (
	"fmt"

	"github.com/shopspring/decimal"
)

// divisionScale is the working precision for rate division. It is far beyond the precision
// of any published risk coefficient, so intermediate results never limit accuracy, while
// staying fixed so two runs of the same computation agree bit for bit.
const divisionScale int32 = 18

// Rate is an exact decimal fraction: 0.0640 means 6.40%.
//
// Rates model APR, PD, LGD, premiums and weights. They are deliberately a distinct type
// from Amount so that a rate can never be added to money by accident.
type Rate struct {
	d decimal.Decimal
}

// NewRate wraps an exact decimal as a Rate.
func NewRate(d decimal.Decimal) Rate { return Rate{d: d} }

// ParseRate parses a decimal literal such as "0.0640".
func ParseRate(s string) (Rate, error) {
	d, err := decimal.NewFromString(s)
	if err != nil {
		return Rate{}, fmt.Errorf("%w: %q", ErrSyntax, s)
	}
	return Rate{d: d}, nil
}

// MustParseRate is ParseRate for compile-time constants and tests.
func MustParseRate(s string) Rate {
	r, err := ParseRate(s)
	if err != nil {
		panic(err)
	}
	return r
}

// RateFromInt builds a whole-number rate, e.g. RateFromInt(1) is 1.0.
func RateFromInt(v int64) Rate { return Rate{d: decimal.NewFromInt(v)} }

// RateFromFraction builds num/den at the package division scale.
func RateFromFraction(num, den int64) (Rate, error) {
	if den == 0 {
		return Rate{}, ErrDivideByZero
	}
	return Rate{d: decimal.NewFromInt(num).DivRound(decimal.NewFromInt(den), divisionScale)}, nil
}

// ZeroRate is the additive identity.
func ZeroRate() Rate { return Rate{d: decimal.Zero} }

// OneRate is the multiplicative identity.
func OneRate() Rate { return Rate{d: decimal.NewFromInt(1)} }

// Decimal exposes the underlying exact value.
func (r Rate) Decimal() decimal.Decimal { return r.d }

// String renders the rate without trailing-zero normalization, preserving the scale the
// value was created with.
func (r Rate) String() string { return r.d.String() }

// StringFixed renders the rate with exactly scale digits, rounding half to even.
func (r Rate) StringFixed(scale int32) string { return r.d.RoundBank(scale).StringFixed(scale) }

// Quantize rounds the rate to scale digits, half to even. Use it to canonicalize a rate
// before hashing or persisting it.
func (r Rate) Quantize(scale int32) Rate { return Rate{d: r.d.RoundBank(scale)} }

// Add returns r + o.
func (r Rate) Add(o Rate) Rate { return Rate{d: r.d.Add(o.d)} }

// Sub returns r - o.
func (r Rate) Sub(o Rate) Rate { return Rate{d: r.d.Sub(o.d)} }

// Mul returns r * o.
func (r Rate) Mul(o Rate) Rate { return Rate{d: r.d.Mul(o.d)} }

// Div returns r / o at the package division scale.
func (r Rate) Div(o Rate) (Rate, error) {
	if o.d.IsZero() {
		return Rate{}, ErrDivideByZero
	}
	return Rate{d: r.d.DivRound(o.d, divisionScale)}, nil
}

// MulInt returns r * v.
func (r Rate) MulInt(v int64) Rate { return Rate{d: r.d.Mul(decimal.NewFromInt(v))} }

// DivInt returns r / v at the package division scale.
func (r Rate) DivInt(v int64) (Rate, error) {
	if v == 0 {
		return Rate{}, ErrDivideByZero
	}
	return Rate{d: r.d.DivRound(decimal.NewFromInt(v), divisionScale)}, nil
}

// Neg returns -r.
func (r Rate) Neg() Rate { return Rate{d: r.d.Neg()} }

// Cmp returns -1, 0 or 1 as r is less than, equal to, or greater than o.
func (r Rate) Cmp(o Rate) int { return r.d.Cmp(o.d) }

// Equal reports numeric equality, ignoring scale: 0.10 equals 0.1.
func (r Rate) Equal(o Rate) bool { return r.d.Equal(o.d) }

// IsZero reports whether the rate is exactly zero.
func (r Rate) IsZero() bool { return r.d.IsZero() }

// IsNegative reports whether the rate is below zero.
func (r Rate) IsNegative() bool { return r.d.IsNegative() }

// IsPositive reports whether the rate is above zero.
func (r Rate) IsPositive() bool { return r.d.IsPositive() }

// Clamp constrains the rate to [lo, hi]. It panics if lo is greater than hi, which is a
// programming error in a risk model's published bounds rather than a runtime condition.
func (r Rate) Clamp(lo, hi Rate) Rate {
	if lo.d.GreaterThan(hi.d) {
		panic("money: Clamp called with lo greater than hi")
	}
	switch {
	case r.d.LessThan(lo.d):
		return lo
	case r.d.GreaterThan(hi.d):
		return hi
	default:
		return r
	}
}

// MinRate returns the smaller of two rates.
func MinRate(a, b Rate) Rate {
	if a.d.LessThan(b.d) {
		return a
	}
	return b
}

// MaxRate returns the larger of two rates.
func MaxRate(a, b Rate) Rate {
	if a.d.GreaterThan(b.d) {
		return a
	}
	return b
}
