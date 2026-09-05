package money

import "errors"

// Sentinel errors returned by the package. Callers match them with errors.Is.
var (
	// ErrInvalidCurrency reports a currency code outside the supported registry.
	ErrInvalidCurrency = errors.New("money: invalid currency")
	// ErrCurrencyMismatch reports an operation on amounts of different currencies.
	ErrCurrencyMismatch = errors.New("money: currency mismatch")
	// ErrOverflow reports an int64 minor-unit overflow.
	ErrOverflow = errors.New("money: amount overflow")
	// ErrPrecisionLoss reports a value that cannot be represented exactly in minor units.
	ErrPrecisionLoss = errors.New("money: value is not representable in minor units")
	// ErrDivideByZero reports a division by a zero rate.
	ErrDivideByZero = errors.New("money: division by zero")
	// ErrSyntax reports a malformed decimal literal.
	ErrSyntax = errors.New("money: invalid decimal syntax")
)
