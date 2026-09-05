package money

import (
	"fmt"
	"strings"
)

// Currency is an ISO 4217 alphabetic currency code.
//
// The MVP registry is deliberately small: an unknown code is rejected at the boundary
// instead of being stored with a guessed exponent.
type Currency string

// Currencies supported by the MVP.
const (
	USD Currency = "USD"
	EUR Currency = "EUR"
	GBP Currency = "GBP"
	JPY Currency = "JPY"
)

// currencyExponents maps a currency to the number of decimal digits in its minor unit.
var currencyExponents = map[Currency]int32{
	USD: 2,
	EUR: 2,
	GBP: 2,
	JPY: 0,
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
