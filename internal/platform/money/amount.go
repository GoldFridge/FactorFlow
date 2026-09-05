package money

import (
	"fmt"

	"github.com/shopspring/decimal"
)

// minInt64 is the one minor-unit value that cannot be negated.
const minInt64 = -1 << 63

var (
	maxInt64Decimal = decimal.NewFromInt(1 << 62).Mul(decimal.NewFromInt(2)).Sub(decimal.NewFromInt(1))
	minInt64Decimal = decimal.NewFromInt(minInt64)
)

// Amount is a quantity of money held as integer minor units of a currency: 972184 USD
// minor units render as "9721.84".
//
// The zero Amount has no currency and is invalid for arithmetic; build amounts with New,
// Zero or Parse.
type Amount struct {
	minor    int64
	currency Currency
}

// New builds an amount from minor units.
func New(minor int64, c Currency) (Amount, error) {
	if !c.IsValid() {
		return Amount{}, fmt.Errorf("%w: %q", ErrInvalidCurrency, string(c))
	}
	return Amount{minor: minor, currency: c}, nil
}

// MustNew is New for constants and tests.
func MustNew(minor int64, c Currency) Amount {
	a, err := New(minor, c)
	if err != nil {
		panic(err)
	}
	return a
}

// Zero returns the zero amount of a currency.
func Zero(c Currency) Amount { return MustNew(0, c) }

// Parse converts an exact decimal literal such as "9721.84" into minor units.
//
// It fails closed: a literal carrying more precision than the currency's minor unit is an
// error rather than a silent rounding, because at an API boundary those extra digits mean
// the caller and the ledger disagree about the amount.
func Parse(s string, c Currency) (Amount, error) {
	if !c.IsValid() {
		return Amount{}, fmt.Errorf("%w: %q", ErrInvalidCurrency, string(c))
	}
	d, err := decimal.NewFromString(s)
	if err != nil {
		return Amount{}, fmt.Errorf("%w: %q", ErrSyntax, s)
	}
	return FromDecimal(d, c)
}

// MustParse is Parse for constants and tests.
func MustParse(s string, c Currency) Amount {
	a, err := Parse(s, c)
	if err != nil {
		panic(err)
	}
	return a
}

// FromDecimal converts an exact decimal, rejecting any value that is not representable in
// minor units.
func FromDecimal(d decimal.Decimal, c Currency) (Amount, error) {
	if !c.IsValid() {
		return Amount{}, fmt.Errorf("%w: %q", ErrInvalidCurrency, string(c))
	}
	shifted := d.Shift(c.Exponent())
	if !shifted.Equal(shifted.Truncate(0)) {
		return Amount{}, fmt.Errorf("%w: %s %s", ErrPrecisionLoss, d.String(), c)
	}
	return fromShifted(shifted, c)
}

// FromDecimalRounded converts an exact decimal, rounding half to even to the currency's
// minor unit. Use it for computed prices; use FromDecimal for values a user supplied.
func FromDecimalRounded(d decimal.Decimal, c Currency) (Amount, error) {
	if !c.IsValid() {
		return Amount{}, fmt.Errorf("%w: %q", ErrInvalidCurrency, string(c))
	}
	return fromShifted(d.Shift(c.Exponent()).RoundBank(0), c)
}

// fromShifted converts an integral minor-unit decimal into an Amount, guarding the int64
// range that the ledger and the solver both rely on.
func fromShifted(shifted decimal.Decimal, c Currency) (Amount, error) {
	if shifted.GreaterThan(maxInt64Decimal) || shifted.LessThan(minInt64Decimal) {
		return Amount{}, fmt.Errorf("%w: %s minor units", ErrOverflow, shifted.String())
	}
	return Amount{minor: shifted.IntPart(), currency: c}, nil
}

// Minor returns the amount in integer minor units.
func (a Amount) Minor() int64 { return a.minor }

// Currency returns the amount's currency.
func (a Amount) Currency() Currency { return a.currency }

// IsValid reports whether the amount carries a supported currency.
func (a Amount) IsValid() bool { return a.currency.IsValid() }

// Decimal returns the amount as an exact decimal in major units.
func (a Amount) Decimal() decimal.Decimal {
	return decimal.New(a.minor, -a.currency.Exponent())
}

// String renders the amount in major units with the currency's fixed precision.
func (a Amount) String() string {
	if !a.currency.IsValid() {
		return "<invalid amount>"
	}
	return a.Decimal().StringFixed(a.currency.Exponent())
}

// Add returns a + o.
func (a Amount) Add(o Amount) (Amount, error) {
	if err := a.assertSameCurrency(o); err != nil {
		return Amount{}, err
	}
	sum := a.minor + o.minor
	if (a.minor > 0 && o.minor > 0 && sum < 0) || (a.minor < 0 && o.minor < 0 && sum >= 0) {
		return Amount{}, fmt.Errorf("%w: %d + %d", ErrOverflow, a.minor, o.minor)
	}
	return Amount{minor: sum, currency: a.currency}, nil
}

// Sub returns a - o.
func (a Amount) Sub(o Amount) (Amount, error) {
	if err := a.assertSameCurrency(o); err != nil {
		return Amount{}, err
	}
	if o.minor == minInt64 {
		return Amount{}, fmt.Errorf("%w: subtracting %d", ErrOverflow, o.minor)
	}
	return a.Add(Amount{minor: -o.minor, currency: o.currency})
}

// Neg returns -a.
func (a Amount) Neg() (Amount, error) {
	if !a.currency.IsValid() {
		return Amount{}, fmt.Errorf("%w: %q", ErrInvalidCurrency, string(a.currency))
	}
	if a.minor == minInt64 {
		return Amount{}, fmt.Errorf("%w: negating %d", ErrOverflow, a.minor)
	}
	return Amount{minor: -a.minor, currency: a.currency}, nil
}

// Abs returns |a|.
func (a Amount) Abs() (Amount, error) {
	if a.minor >= 0 {
		if !a.currency.IsValid() {
			return Amount{}, fmt.Errorf("%w: %q", ErrInvalidCurrency, string(a.currency))
		}
		return a, nil
	}
	return a.Neg()
}

// Mul multiplies the amount by a rate, rounding half to even to the minor unit.
func (a Amount) Mul(r Rate) (Amount, error) {
	if !a.currency.IsValid() {
		return Amount{}, fmt.Errorf("%w: %q", ErrInvalidCurrency, string(a.currency))
	}
	return FromDecimalRounded(a.Decimal().Mul(r.Decimal()), a.currency)
}

// MulInt multiplies the amount by an integer factor, exactly.
func (a Amount) MulInt(v int64) (Amount, error) {
	if !a.currency.IsValid() {
		return Amount{}, fmt.Errorf("%w: %q", ErrInvalidCurrency, string(a.currency))
	}
	return FromDecimal(a.Decimal().Mul(decimal.NewFromInt(v)), a.currency)
}

// Div divides the amount by a rate, rounding half to even to the minor unit.
func (a Amount) Div(r Rate) (Amount, error) {
	if !a.currency.IsValid() {
		return Amount{}, fmt.Errorf("%w: %q", ErrInvalidCurrency, string(a.currency))
	}
	if r.IsZero() {
		return Amount{}, ErrDivideByZero
	}
	q := a.Decimal().DivRound(r.Decimal(), a.currency.Exponent()+divisionScale)
	return FromDecimalRounded(q, a.currency)
}

// RateAgainst returns a / o as a Rate, for ratios such as an allocation's share of supply.
func (a Amount) RateAgainst(o Amount) (Rate, error) {
	if err := a.assertSameCurrency(o); err != nil {
		return Rate{}, err
	}
	if o.minor == 0 {
		return Rate{}, ErrDivideByZero
	}
	num := decimal.NewFromInt(a.minor)
	den := decimal.NewFromInt(o.minor)
	return Rate{d: num.DivRound(den, divisionScale)}, nil
}

// Cmp returns -1, 0 or 1 as a is less than, equal to, or greater than o.
func (a Amount) Cmp(o Amount) (int, error) {
	if err := a.assertSameCurrency(o); err != nil {
		return 0, err
	}
	switch {
	case a.minor < o.minor:
		return -1, nil
	case a.minor > o.minor:
		return 1, nil
	default:
		return 0, nil
	}
}

// Equal reports whether both amounts have the same currency and minor units.
func (a Amount) Equal(o Amount) bool {
	return a.currency == o.currency && a.minor == o.minor
}

// IsZero reports whether the amount is exactly zero.
func (a Amount) IsZero() bool { return a.minor == 0 }

// IsPositive reports whether the amount is above zero.
func (a Amount) IsPositive() bool { return a.minor > 0 }

// IsNegative reports whether the amount is below zero.
func (a Amount) IsNegative() bool { return a.minor < 0 }

// Sum adds amounts left to right. An empty list is an error because the result would have
// no currency to carry.
func Sum(amounts ...Amount) (Amount, error) {
	if len(amounts) == 0 {
		return Amount{}, fmt.Errorf("%w: empty sum", ErrInvalidCurrency)
	}
	total := amounts[0]
	if !total.IsValid() {
		return Amount{}, fmt.Errorf("%w: %q", ErrInvalidCurrency, string(total.currency))
	}
	for _, next := range amounts[1:] {
		var err error
		if total, err = total.Add(next); err != nil {
			return Amount{}, err
		}
	}
	return total, nil
}

// Min returns the smaller of two amounts.
func Min(a, b Amount) (Amount, error) {
	cmp, err := a.Cmp(b)
	if err != nil {
		return Amount{}, err
	}
	if cmp <= 0 {
		return a, nil
	}
	return b, nil
}

// Max returns the larger of two amounts.
func Max(a, b Amount) (Amount, error) {
	cmp, err := a.Cmp(b)
	if err != nil {
		return Amount{}, err
	}
	if cmp >= 0 {
		return a, nil
	}
	return b, nil
}

func (a Amount) assertSameCurrency(o Amount) error {
	if !a.currency.IsValid() {
		return fmt.Errorf("%w: %q", ErrInvalidCurrency, string(a.currency))
	}
	if a.currency != o.currency {
		return fmt.Errorf("%w: %s and %s", ErrCurrencyMismatch, a.currency, o.currency)
	}
	return nil
}
