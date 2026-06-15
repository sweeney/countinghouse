// Package round is the single source of truth for countinghouse's
// response-boundary numeric rounding: the decimal-place policy and a
// NaN/Inf-safe rounding helper, shared by the energy assembly layer and the
// httpapi handlers so the two can never disagree on precision or on how a
// non-finite value is presented.
//
// Influx increase()/integral() and the cost multiply produce long float tails
// (e.g. 0.24127950000000187); values are rounded here at the boundary so
// consumers see tidy numbers.
package round

import "math"

// Decimal places for the values countinghouse emits.
const (
	KWhDP   = 3 // kWh — ~Wh resolution.
	MoneyDP = 4 // money (£) — sub-penny, keeps tiny per-device costs meaningful.
	WDP     = 1 // watts.
	CovDP   = 4 // coverage / duty fractions.
)

// To rounds x to dp decimal places (half away from zero, so negative inputs —
// e.g. a tiny negative counter delta from a noisy reset — round symmetrically).
//
// Non-finite inputs (NaN, ±Inf) round to 0. This is the single agreed policy:
// encoding/json cannot marshal NaN/±Inf, so letting one reach the encoder would
// produce a truncated 200 response; sanitising to 0 at the rounding boundary
// keeps every emitted value finite and JSON-safe.
func To(x float64, dp int) float64 {
	if math.IsNaN(x) || math.IsInf(x, 0) {
		return 0
	}
	p := math.Pow10(dp)
	return math.Round(x*p) / p
}
