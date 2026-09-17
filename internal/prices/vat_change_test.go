package prices

import (
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// The temporary zero rate of VAT on domestic electricity in Great Britain:
// 0% for supplies from 1 October 2026 to 31 March 2027, 5% either side.
//
// This is the case that moved the VAT check from Gate A to Gate B. Under a Gate A
// rejection, the morning the zero rate took effect every slot the supplier
// published would have been refused — because our config still said 5% — and the
// archive would have stopped filling while /healthz reported fetches succeeding.
// The window in which prices matter most would have been the window with no
// prices.
//
// So the behaviour under test is: a stale config must produce NOISE, not LOSS.
// ---------------------------------------------------------------------------

func vatLondon(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

// zeroRated builds slots priced as the supplier will price them once the zero
// rate is in force: inc-VAT equals ex-VAT, because the VAT is nil.
func zeroRated(start time.Time, exc ...float64) []Slot {
	// The archive stores UTC, and Gate A rejects anything else outright. Tests here
	// are written at LOCAL wall-clock instants because that is how the statute reads
	// ("from 1 October"), so convert rather than pretend the zone does not matter.
	start = start.UTC()
	out := make([]Slot, 0, len(exc))
	for i, v := range exc {
		s := start.Add(time.Duration(i) * SlotLength)
		e := s.Add(SlotLength)
		out = append(out, Slot{
			TariffCode: "E-1R-AGILE-24-10-01-A", ValidFrom: s, ValidTo: &e,
			ExcVATPence: v, IncVATPence: v, RetrievedAt: start,
		})
	}
	return out
}

// standardRated builds slots at 5%.
func standardRated(start time.Time, exc ...float64) []Slot {
	out := zeroRated(start, exc...)
	for i := range out {
		out[i].IncVATPence = out[i].ExcVATPence * 1.05
	}
	return out
}

// THE headline case. Config has not been updated, the supplier has zero-rated the
// prices, and every slot disagrees with what we expect. Nothing may be rejected.
func TestZeroRateAgainstStaleConfigWarnsButNeverRejects(t *testing.T) {
	loc := vatLondon(t)
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, loc)
	slots := zeroRated(start, 12, 14, 16, 18, 20, 22, 24, 26)

	five := 0.05
	res := Validate(slots, ValidateOptions{ExpectVATRate: &five}) // config still says 5%

	if len(res.Rejected) != 0 {
		t.Fatalf("%d slots rejected; a VAT change must never cost us prices: %+v",
			len(res.Rejected), res.Rejected)
	}
	if len(res.Accepted) != len(slots) {
		t.Fatalf("accepted %d of %d slots", len(res.Accepted), len(slots))
	}

	var vatWarnings int
	for _, w := range res.Warnings {
		if w.Kind == WarnVATMismatch {
			vatWarnings++
		}
	}
	if vatWarnings != len(slots) {
		t.Errorf("got %d vat_mismatch warnings, want %d — one per slot", vatWarnings, len(slots))
	}
}

// The warning has to say what the supplier is ACTUALLY charging, because that is
// what an operator needs in order to correct the configuration. A warning that
// only says "mismatch" makes somebody go and work out the rate by hand.
func TestVATWarningNamesTheImpliedRate(t *testing.T) {
	loc := vatLondon(t)
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, loc)
	five := 0.05

	res := Validate(zeroRated(start, 20), ValidateOptions{ExpectVATRate: &five})
	if len(res.Warnings) != 1 {
		t.Fatalf("want exactly one warning, got %d", len(res.Warnings))
	}
	// 20p inc on 20p exc implies VAT of 0.0000.
	if !contains(res.Warnings[0].Detail, "imply VAT of 0.0000") {
		t.Errorf("warning does not name the implied rate: %q", res.Warnings[0].Detail)
	}
}

// With config updated, the same prices are unremarkable — no warning at all.
// This is the test that proves the check still WORKS after the change, rather
// than having been quietly defanged.
func TestZeroRateAgainstCorrectConfigIsSilent(t *testing.T) {
	loc := vatLondon(t)
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, loc)
	zero := 0.0

	res := Validate(zeroRated(start, 12, 14, 16), ValidateOptions{ExpectVATRate: &zero})
	for _, w := range res.Warnings {
		if w.Kind == WarnVATMismatch {
			t.Errorf("unexpected vat_mismatch under correct config: %s", w.Detail)
		}
	}
}

// A zero VAT rate must be taken LITERALLY, not read as "unset". This is the trap
// in the whole feature: `vat_rate: 0` in a config document is now a real, correct
// value for six months, and any code treating 0 as absent would silently stop
// checking VAT for exactly that period.
func TestZeroIsARealVATRateNotAnUnsetOne(t *testing.T) {
	loc := vatLondon(t)
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, loc)
	zero := 0.0

	// Prices still carrying 5% while config correctly says 0% — the supplier being
	// wrong, or us reading the wrong tariff. It must still be caught.
	res := Validate(standardRated(start, 20), ValidateOptions{ExpectVATRate: &zero})

	var found bool
	for _, w := range res.Warnings {
		if w.Kind == WarnVATMismatch {
			found = true
		}
	}
	if !found {
		t.Error("expected a vat_mismatch: 21p inc on 20p exc is not zero-rated, " +
			"but an ExpectVATRate of 0 was treated as 'no opinion'")
	}
}

// The per-slot resolver is what makes a batch spanning the transition work: one
// fetch can straddle 1 October, and each slot must be checked against the rate in
// force for ITS OWN valid_from, not the rate in force when the process started.
func TestBatchSpanningTheTransitionChecksEachSlotAgainstItsOwnRate(t *testing.T) {
	loc := vatLondon(t)
	boundary := time.Date(2026, 10, 1, 0, 0, 0, 0, loc)

	// Four slots: two before the change at 5%, two after it at 0%. All correct.
	before := standardRated(boundary.Add(-2*SlotLength), 30, 32)
	after := zeroRated(boundary, 34, 36)
	slots := append(before, after...)

	res := Validate(slots, ValidateOptions{
		ExpectVATRateAt: func(at time.Time) (float64, bool) {
			if at.Before(boundary) {
				return 0.05, true
			}
			return 0, true
		},
	})

	if len(res.Rejected) != 0 {
		t.Fatalf("rejected %d slots: %+v", len(res.Rejected), res.Rejected)
	}
	for _, w := range res.Warnings {
		if w.Kind == WarnVATMismatch {
			t.Errorf("every slot is correct for its own date, but got: %s", w.Detail)
		}
	}

	// And the reverse: a single frozen rate misjudges half the batch. This is what
	// the scalar VATRate would have done.
	five := 0.05
	frozen := Validate(slots, ValidateOptions{ExpectVATRate: &five})
	var n int
	for _, w := range frozen.Warnings {
		if w.Kind == WarnVATMismatch {
			n++
		}
	}
	if n != 2 {
		t.Errorf("a frozen 5%% rate should misjudge the 2 zero-rated slots, flagged %d", n)
	}
}

// 1 April 2027: the rate goes back UP. The second transition is the one that gets
// forgotten, and its failure mode is the opposite — config left at 0% while the
// supplier resumes charging 5%.
func TestReturnToStandardRateIsAlsoOnlyAWarning(t *testing.T) {
	loc := vatLondon(t)
	start := time.Date(2027, 4, 1, 0, 0, 0, 0, loc)
	zero := 0.0

	res := Validate(standardRated(start, 28, 30, 32), ValidateOptions{ExpectVATRate: &zero})

	if len(res.Rejected) != 0 {
		t.Fatalf("rejected %d slots on the way back up: %+v", len(res.Rejected), res.Rejected)
	}
	if len(res.Accepted) != 3 {
		t.Fatalf("accepted %d of 3", len(res.Accepted))
	}
	var n int
	for _, w := range res.Warnings {
		if w.Kind == WarnVATMismatch {
			n++
		}
	}
	if n != 3 {
		t.Errorf("got %d vat_mismatch warnings, want 3", n)
	}
}

// Zero-rated PLUNGE prices: a negative price at 0% VAT stays exactly itself. The
// gross-up that makes a negative price more negative has nothing to do, and the
// slot must not be mistaken for a mismatch.
func TestZeroRatedPlungePricesAreUnchangedAndUnflagged(t *testing.T) {
	loc := vatLondon(t)
	start := time.Date(2026, 10, 1, 12, 0, 0, 0, loc)
	zero := 0.0

	res := Validate(zeroRated(start, -2.625, -1.5, 0), ValidateOptions{ExpectVATRate: &zero})
	if len(res.Rejected) != 0 {
		t.Fatalf("rejected a zero-rated plunge slot: %+v", res.Rejected)
	}
	for _, w := range res.Warnings {
		if w.Kind == WarnVATMismatch {
			t.Errorf("zero-rated plunge slot flagged: %s", w.Detail)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
