package energy

import "time"

// ---------------------------------------------------------------------------
// Counterfactuals (issue #36 friction #3).
//
// Nearly every question asked of this service reduced to "what would this window
// have cost under X?" — the same kWh on a named fixed tariff, on a rate from a
// renewal letter, or spread flat across the window's own prices. Those were
// hand-rolled five times against the API, and two baseline errors came out of it:
// comparing against a tariff that had already EXPIRED, and omitting the
// STANDING-CHARGE difference.
//
// Both are the kind of error a server-side primitive makes structurally hard
// rather than merely documented against. The service already holds the dated
// agreements, the price curve and the consumption; the client's only genuine
// contribution is the alternative to compare against.
//
// Two shape decisions carry that weight:
//
//   - `delta` is split into energy / standing / total, with
//     energy + standing == total asserted. You cannot omit the standing charge
//     when it has its own field and the totals will not reconcile without it.
//   - `available_now` is a labelled field on every alternative, so an expired
//     baseline is a statement rather than a silence.
// ---------------------------------------------------------------------------

// Comparison scopes. REQUIRED on the wire, with no default, because the choice
// changes the answer by enough to support opposite conclusions.
//
// Measured on one real window: automation that is transformative on the load it
// controls (−31.8% against a flat rate over the monitored devices) is nearly
// invisible against the whole house (−0.4%), because unmonitored evening cooking
// swamps it. Both numbers are correct. A default would pick one of them silently,
// once, for every caller, forever.
const (
	// ScopeHousehold compares the whole home: the meter's energy.
	ScopeHousehold = "household"

	// ScopeMonitored compares only the devices this service meters individually.
	ScopeMonitored = "monitored"
)

// ValidScope reports whether s is an accepted comparison scope.
func ValidScope(s string) bool { return s == ScopeHousehold || s == ScopeMonitored }

// Costed is one priced view of a window: what the energy came to, what the
// standing charge came to, and their total. All VAT-inclusive £.
type Costed struct {
	// Label is what to call this in a UI. "actual" for the real bill.
	Label string `json:"label"`

	// TariffCodes names the tariffs behind it, when they are known. Plural for
	// the same reason /series carries a plural: a window may span a switchover.
	TariffCodes []string `json:"tariff_codes,omitempty"`

	KWh            float64 `json:"kwh"`
	EnergyCost     float64 `json:"energy_cost"`
	StandingCharge float64 `json:"standing_charge"`
	Total          float64 `json:"total"`
}

// Alternative is one counterfactual, and how it compares.
type Alternative struct {
	Costed

	// Kind is how it was derived — see the Kind constants.
	Kind string `json:"kind"`

	// AvailableNow reports whether this is something that could be bought today,
	// derived from the agreement's dates against the service clock.
	//
	// A POINTER because three states matter and two of them are not "false": a
	// dated agreement that has lapsed (false), one on sale (true), and a
	// counterfactual to which availability does not apply at all — a rate from a
	// letter, or the window's own mean — which is nil and omitted.
	//
	// This field is the expired-baseline error, made impossible to miss.
	AvailableNow  *bool  `json:"available_now,omitempty"`
	AvailableNote string `json:"available_note,omitempty"`

	// Delta is actual MINUS this alternative, per component. Negative means the
	// actual was cheaper.
	Delta Delta `json:"delta"`

	// Verdict reads the sign of Delta.Total so a consumer does not have to decide
	// which direction is good.
	Verdict string `json:"verdict"`
}

// Delta is the difference between the actual and an alternative, split so the
// standing charge cannot be quietly dropped.
//
// Energy + Standing == Total, always. That is not a convenience: omitting the
// standing-charge difference was one of the two baseline errors this endpoint
// exists to prevent, and a total that will not reconcile without it is a much
// stronger guarantee than a sentence in the docs.
type Delta struct {
	Energy   float64 `json:"energy"`
	Standing float64 `json:"standing"`
	Total    float64 `json:"total"`
}

// Verdicts.
const (
	VerdictActualCheaper      = "actual_cheaper"
	VerdictAlternativeCheaper = "alternative_cheaper"
	VerdictLevel              = "level"
)

// Alternative kinds.
const (
	// KindFlat: rates supplied by the caller — the renewal-letter case.
	KindFlat = "flat"

	// KindTariff: a dated agreement this service already knows about.
	KindTariff = "tariff"

	// KindWindowMean: the same kWh spread flat across the window's own prices.
	KindWindowMean = "window_mean"
)

// CompareTo builds an Alternative from the actual and a costed counterfactual.
//
// The single place a delta is computed, so the reconciliation invariant cannot be
// broken by one call site getting it wrong.
func CompareTo(actual, alt Costed, kind string, availableNow *bool, note string) Alternative {
	d := Delta{
		Energy:   actual.EnergyCost - alt.EnergyCost,
		Standing: actual.StandingCharge - alt.StandingCharge,
	}
	// Summed from the components rather than from the totals, so the invariant
	// holds by construction rather than by agreement between two subtractions.
	d.Total = d.Energy + d.Standing

	return Alternative{
		Costed:        alt,
		Kind:          kind,
		AvailableNow:  availableNow,
		AvailableNote: note,
		Delta:         d,
		Verdict:       verdictFor(d.Total),
	}
}

// verdictFor reads the sign of a total delta.
//
// The dead band is one hundredth of a penny: two tariffs that differ by less than
// that over a window are level, and reporting a winner there would be noise
// dressed as a finding.
func verdictFor(total float64) string {
	const epsilon = 1e-4
	switch {
	case total < -epsilon:
		return VerdictActualCheaper
	case total > epsilon:
		return VerdictAlternativeCheaper
	default:
		return VerdictLevel
	}
}

// FlatCost prices kwh and a window's days at one ex-VAT unit rate and daily
// standing charge, grossing both up by vatRate.
//
// days is fractional on purpose: a comparison over a part-day window that
// charged a whole day's standing charge would flatter or penalise the
// alternative for a reason that has nothing to do with the tariff.
func FlatCost(label string, kwh, days, unitRate, dailyStanding, vatRate float64) Costed {
	mult := 1 + vatRate
	energy := kwh * unitRate * mult
	standing := days * dailyStanding * mult
	return Costed{
		Label:          label,
		KWh:            kwh,
		EnergyCost:     energy,
		StandingCharge: standing,
		Total:          energy + standing,
	}
}

// WindowDays is the window's length in fractional days.
func WindowDays(w Window) float64 { return w.Stop.Sub(w.Start).Hours() / 24 }

// MeanRateOverWindow returns the time-weighted mean VAT-inclusive £/kWh across
// the whole window, and whether every interval in it was priced.
//
// This is the "same kWh, spread flat across this window's own prices" baseline.
// Time-weighted rather than energy-weighted for the same reason the per-bucket
// array is: an energy-weighted mean is just the effective rate already paid, so
// comparing against it would always report level and answer nothing.
//
// Incomplete means some interval held no rate, and the caller must refuse rather
// than average what it happens to have — a mean over the priced half of a window
// is a plausible-looking wrong number.
func MeanRateOverWindow(w Window, p Pricer) (rate float64, complete bool) {
	if p == nil {
		return 0, false
	}
	ri := rateInterval(p)
	if ri <= 0 {
		// A rate that never varies IS its own mean.
		return singleRate(w.Start, p)
	}
	return meanRateOver(w.Start, w.Stop, ri, p)
}

func singleRate(at time.Time, p Pricer) (float64, bool) { return p.RateAt(at) }
