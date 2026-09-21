package energy

import (
	"math"
	"testing"
	"time"
)

func boolp(b bool) *bool { return &b }

// The invariant the whole shape rests on: you cannot drop the standing charge,
// because it has its own field and the total will not reconcile without it.
// Omitting it was one of the two baseline errors /compare exists to prevent.
func TestDeltaComponentsReconcileToTheTotal(t *testing.T) {
	actual := Costed{Label: "actual", EnergyCost: 61.9237, StandingCharge: 11.832, Total: 73.7557}
	alt := Costed{Label: "Flexible", EnergyCost: 91.7599, StandingCharge: 11.1174, Total: 102.8773}

	got := CompareTo(actual, alt, KindTariff, boolp(false), "ended 2026-03-01")

	if math.Abs(got.Delta.Energy-(-29.8362)) > 1e-9 {
		t.Errorf("delta.energy = %v, want -29.8362", got.Delta.Energy)
	}
	if math.Abs(got.Delta.Standing-0.7146) > 1e-9 {
		t.Errorf("delta.standing = %v, want +0.7146", got.Delta.Standing)
	}
	if sum := got.Delta.Energy + got.Delta.Standing; math.Abs(got.Delta.Total-sum) > 1e-12 {
		t.Errorf("delta.total = %v but components sum to %v", got.Delta.Total, sum)
	}
	if got.Verdict != VerdictActualCheaper {
		t.Errorf("verdict = %q, want %q", got.Verdict, VerdictActualCheaper)
	}
	// The expired baseline is a labelled field, not a silence.
	if got.AvailableNow == nil || *got.AvailableNow {
		t.Errorf("available_now = %v, want false", got.AvailableNow)
	}
	if got.AvailableNote == "" {
		t.Error("an unavailable alternative should say why")
	}
}

func TestVerdictReadsTheSignOfTheTotal(t *testing.T) {
	base := Costed{EnergyCost: 10, StandingCharge: 1, Total: 11}
	for _, tc := range []struct {
		name string
		alt  Costed
		want string
	}{
		{"alternative dearer", Costed{EnergyCost: 20, StandingCharge: 1, Total: 21}, VerdictActualCheaper},
		{"alternative cheaper", Costed{EnergyCost: 5, StandingCharge: 1, Total: 6}, VerdictAlternativeCheaper},
		{"identical", base, VerdictLevel},
		// Inside the dead band: reporting a winner here would be noise dressed
		// as a finding.
		{"immaterial", Costed{EnergyCost: 10.00001, StandingCharge: 1, Total: 11.00001}, VerdictLevel},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := CompareTo(base, tc.alt, KindFlat, nil, "").Verdict; got != tc.want {
				t.Errorf("verdict = %q, want %q", got, tc.want)
			}
		})
	}
}

// Availability does not apply to every counterfactual — a rate off a letter is
// not something with dates — so the field is a pointer and omitted there.
func TestAvailabilityIsOmittedWhereItDoesNotApply(t *testing.T) {
	got := CompareTo(Costed{}, Costed{}, KindFlat, nil, "")
	if got.AvailableNow != nil {
		t.Errorf("available_now = %v, want nil for a caller-supplied rate", *got.AvailableNow)
	}
}

// Both components are grossed up, and days is fractional: charging a whole day's
// standing charge over a part-day window would flatter or penalise the
// alternative for a reason unrelated to the tariff.
func TestFlatCostGrossesUpBothComponents(t *testing.T) {
	c := FlatCost("Renewal quote", 100, 2.5, 0.20, 0.50, 0.05)

	if want := 100 * 0.20 * 1.05; math.Abs(c.EnergyCost-want) > 1e-9 {
		t.Errorf("energy = %v, want %v", c.EnergyCost, want)
	}
	if want := 2.5 * 0.50 * 1.05; math.Abs(c.StandingCharge-want) > 1e-9 {
		t.Errorf("standing = %v, want %v (fractional days)", c.StandingCharge, want)
	}
	if math.Abs(c.Total-(c.EnergyCost+c.StandingCharge)) > 1e-12 {
		t.Error("total must be the sum of its parts")
	}
}

func TestWindowDaysIsFractional(t *testing.T) {
	start := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)
	w := Window{Start: start, Stop: start.Add(36 * time.Hour)}
	if got := WindowDays(w); math.Abs(got-1.5) > 1e-9 {
		t.Errorf("WindowDays = %v, want 1.5", got)
	}
}

// "The same kWh spread flat across this window's own prices" is a TIME-weighted
// mean. Energy-weighted would be the effective rate already paid, so comparing
// against it would always report level and answer nothing.
func TestMeanRateOverWindowIsTimeWeighted(t *testing.T) {
	start := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)
	w := Window{Start: start, Stop: start.Add(2 * time.Hour)}
	p := halfHours(start, 0.10, 0.20, 0.30, 0.40)

	rate, complete := MeanRateOverWindow(w, p)
	if !complete {
		t.Fatal("every slot is held; the mean should be complete")
	}
	if want := 0.25; math.Abs(rate-want) > 1e-9 {
		t.Errorf("mean = %v, want %v", rate, want)
	}
}

// A mean over the priced half of a window is a plausible-looking wrong number.
func TestMeanRateOverWindowRefusesAPartialWindow(t *testing.T) {
	start := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)
	w := Window{Start: start, Stop: start.Add(2 * time.Hour)}
	p := halfHours(start, 0.10, 0.20) // only the first hour is held

	if _, complete := MeanRateOverWindow(w, p); complete {
		t.Error("a window with unpriced slots has no honest mean")
	}
}

// A flat tariff never varies, so it IS its own mean.
func TestMeanRateOverWindowOnAFlatTariff(t *testing.T) {
	start := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)
	w := Window{Start: start, Stop: start.Add(48 * time.Hour)}

	rate, complete := MeanRateOverWindow(w, FlatPricer{RatePerKWh: 0.2089, known: true})
	if !complete || math.Abs(rate-0.2089) > 1e-9 {
		t.Errorf("mean = %v (complete=%v), want the flat rate", rate, complete)
	}
}

func TestValidScope(t *testing.T) {
	for _, s := range []string{ScopeHousehold, ScopeMonitored} {
		if !ValidScope(s) {
			t.Errorf("ValidScope(%q) = false", s)
		}
	}
	for _, s := range []string{"", "device", "house", "HOUSEHOLD"} {
		if ValidScope(s) {
			t.Errorf("ValidScope(%q) = true, want false", s)
		}
	}
}
