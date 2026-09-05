// Package money provides the exact arithmetic primitives every financial decision in
// FactorFlow is built on.
//
// Two rules are enforced by the type system rather than by convention:
//
//   - Amounts are integer minor units of a known currency. float64 is never used, and an
//     Amount cannot be combined with an Amount of another currency.
//   - Rates (APR, PD, LGD, premiums) are exact decimals, never percentages-as-integers,
//     and cannot be added to an Amount.
//
// Whenever a result is not representable in minor units the caller must say how to round.
// Rounding is banker's rounding (half to even), matching the specification's display rule.
package money
