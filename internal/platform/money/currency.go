package money

import (
	"fmt"
	"strings"
)

// Currency is a unit of account: an ISO 4217 code, or a network's own unit.
//
// The registry is deliberately small: an unknown code is rejected at the boundary instead
// of being stored with a guessed exponent. Getting an exponent wrong is not a rounding
// error, it is a factor of a hundred or of a hundred million.
type Currency string

// Currencies supported by the MVP.
const (
	USD Currency = "USD"
	EUR Currency = "EUR"
	GBP Currency = "GBP"
	JPY Currency = "JPY"
	// HBAR is Hedera's own unit. It is here because a machine paying for an answer pays in
	// what the network moves, and quoting that price in dollars would mean inventing an
	// exchange rate somewhere — which is the kind of number this package exists to refuse.
	// Its minor unit is the tinybar, and eight digits is exactly what the network uses.
	HBAR Currency = "HBAR"
)

// currencyExponents maps a currency to the number of decimal digits in its minor unit.
var currencyExponents = map[Currency]int32{
	USD:  2,
	EUR:  2,
	GBP:  2,
	JPY:  0,
	HBAR: 8,
}

// ParseCurrency validates and normalizes a currency code.
func ParseCurrency(s string) (Currency, error) {
	c := Currency(strings.ToUpper(strings.TrimSpace(s)))
	if !c.IsValid() {
		return "", fmt.Errorf("%w: %q", ErrInvalidCurrency, s)
	}
	return c, nil
}

// IsValid reports whether the currency is in the supported registry.
func (c Currency) IsValid() bool {
	_, ok := currencyExponents[c]
	return ok
}

// Exponent returns the number of decimal digits in the currency's minor unit.
// It panics for unsupported currencies; use IsValid or ParseCurrency first.
func (c Currency) Exponent() int32 {
	exp, ok := currencyExponents[c]
	if !ok {
		panic(fmt.Sprintf("money: exponent requested for unsupported currency %q", string(c)))
	}
	return exp
}

// String returns the alphabetic code.
func (c Currency) String() string { return string(c) }
