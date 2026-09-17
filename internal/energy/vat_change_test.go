package energy

import (
	"math"
	"testing"
	"time"

	"github.com/sweeney/countinghouse/internal/config"
)

// ---------------------------------------------------------------------------
// The temporary zero rate of VAT on domestic electricity in Great Britain:
// 0% for supplies from 1 October 2026 to 31 March 2027, 5% either side.
//
// On a half-hourly tariff the ENERGY cost needs no help across this change: the
// price archive holds the supplier's own inc-VAT figures, so the new rate simply
// arrives in the prices. The STANDING CHARGE is the one number that really is
// grossed up from the config rate, which makes it the only part of a bill that
// gets this wrong if the agreement is not split.
// ---------------------------------------------------------------------------

func vatLoc(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

// A bill spanning 1 October charges the standing charge at 5% up to the boundary
// and at 0% after it, apportioned by real elapsed days.
func TestStandingChargeAcrossTheZeroRateBoundary(t *testing.T) {
	loc := vatLoc(t)
	boundary := time.Date(2026, 10, 1, 0, 0, 0, 0, loc)

	const daily = 0.59
	segs := []config.Segment{
		{ // two days at 5%
			Start: time.Date(2026, 9, 29, 0, 0, 0, 0, loc), Stop: boundary,
			Tariff: config.Tariff{DailyStandingCharge: daily, VATRate: 0.05},
		},
		{ // three days at 0%
			Start: boundary, Stop: time.Date(2026, 10, 4, 0, 0, 0, 0, loc),
			Tariff: config.Tariff{DailyStandingCharge: daily, VATRate: 0},
		},
	}

	got := StandingChargeAcross(segs)
	want := 2*daily*1.05 + 3*daily*1.00 // 1.239 + 1.77
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("standing charge %.6f, want %.6f", got, want)
	}

	// And the thing the split exists to prevent: charging the whole span at 5%.
	if wrong := 5 * daily * 1.05; math.Abs(got-wrong) < 1e-9 {
		t.Error("the whole window was charged at 5%; the VAT change was not applied")
	}
}

// The zero rate makes the standing charge exactly its ex-VAT value. Worth pinning
// because a Multiplier() of 1 is the case where a missing gross-up and a correct
// one look identical — so this is the period in which such a bug would hide.
func TestZeroRatedStandingChargeIsItsExVATValue(t *testing.T) {
	loc := vatLoc(t)
	seg := config.Segment{
		Start:  time.Date(2026, 12, 1, 0, 0, 0, 0, loc),
		Stop:   time.Date(2026, 12, 31, 0, 0, 0, 0, loc),
		Tariff: config.Tariff{DailyStandingCharge: 0.59, VATRate: 0},
	}
	got := StandingChargeAcross([]config.Segment{seg})
	if want := 30 * 0.59; math.Abs(got-want) > 1e-9 {
		t.Errorf("standing charge %.6f, want %.6f (30 days, no VAT)", got, want)
	}
}

// The transition back UP on 1 April 2027, apportioned the same way. Included
// because the return to 5% is the half of this change that will not be front of
// mind by the time it happens.
func TestStandingChargeAcrossTheReturnToStandardRate(t *testing.T) {
	loc := vatLoc(t)
	boundary := time.Date(2027, 4, 1, 0, 0, 0, 0, loc)

	const daily = 0.59
	segs := []config.Segment{
		{Start: time.Date(2027, 3, 30, 0, 0, 0, 0, loc), Stop: boundary,
			Tariff: config.Tariff{DailyStandingCharge: daily, VATRate: 0}},
		{Start: boundary, Stop: time.Date(2027, 4, 2, 0, 0, 0, 0, loc),
			Tariff: config.Tariff{DailyStandingCharge: daily, VATRate: 0.05}},
	}

	got := StandingChargeAcross(segs)
	if want := 2*daily*1.00 + 1*daily*1.05; math.Abs(got-want) > 1e-9 {
		t.Errorf("standing charge %.6f, want %.6f", got, want)
	}
}

// A zero VAT rate must survive the config type as a real value. `Multiplier()` is
// the single place cost math grosses up, and 1 + 0 must be 1 — not a signal to
// substitute a default.
func TestZeroVATMultiplierIsExactlyOne(t *testing.T) {
	if got := (config.Tariff{VATRate: 0}).Multiplier(); got != 1 {
		t.Errorf("Multiplier() = %v for a zero-rated tariff, want exactly 1", got)
	}
	if got := (config.Tariff{VATRate: 0.05}).Multiplier(); got != 1.05 {
		t.Errorf("Multiplier() = %v, want 1.05", got)
	}
}

// A zero-rated PLUNGE price stays exactly itself. Under 5% a negative price is
// grossed to something MORE negative; at 0% that adjustment vanishes, and code
// that special-cased the sign would show it here.
func TestZeroRatedNegativePriceIsUnchanged(t *testing.T) {
	zero := config.Tariff{UnitRate: -0.02, VATRate: 0}
	if got := -0.02 * zero.Multiplier(); math.Abs(got-(-0.02)) > 1e-12 {
		t.Errorf("zero-rated negative rate became %v, want -0.02", got)
	}
	five := config.Tariff{UnitRate: -0.02, VATRate: 0.05}
	if got := -0.02 * five.Multiplier(); got >= -0.02 {
		t.Errorf("VAT on a negative rate gave %v; it must become MORE negative", got)
	}
}
