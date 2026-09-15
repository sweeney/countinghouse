package prices

import (
	"context"
	"math"
	"testing"
	"time"
)

func scStore(t *testing.T) *SQLiteStore {
	t.Helper()
	st, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() }) //nolint:errcheck
	return st
}

func day(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

func charge(from time.Time, to *time.Time, exc, inc float64) DailyCharge {
	return DailyCharge{
		TariffCode: "E-1R-AGILE-24-10-01-A", ValidFrom: from, ValidTo: to,
		ExcVATPence: exc, IncVATPence: inc, RetrievedAt: from,
	}
}

// The archive round-trips, and an OPEN-ENDED charge survives as open-ended. That
// is the normal state of the current standing charge, not an edge case, and
// coercing nil to a zero time would make a live rate look long expired.
func TestStandingChargesRoundTripIncludingOpenEnded(t *testing.T) {
	st := scStore(t)
	ctx := context.Background()

	in := []DailyCharge{
		charge(day(2026, 1, 1), ptr(day(2026, 10, 1)), 56.19, 59.0),
		charge(day(2026, 10, 1), nil, 56.19, 56.19), // zero-rated, still current
	}
	if _, err := st.PutStandingCharges(ctx, in); err != nil {
		t.Fatal(err)
	}

	got, err := st.StandingCharges(ctx, "E-1R-AGILE-24-10-01-A", day(2025, 1, 1), day(2027, 1, 1))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d charges, want 2", len(got))
	}
	if got[1].ValidTo != nil {
		t.Errorf("open-ended charge came back with valid_to %v", got[1].ValidTo)
	}
	if got[1].IncVATPence != 56.19 {
		t.Errorf("inc %v, want 56.19", got[1].IncVATPence)
	}
}

// Writes are idempotent, and a changed value is a RESTATEMENT — the same
// contract unit prices get, because a standing charge moves every bill it has
// ever touched.
func TestStandingChargeWritesAreIdempotentAndDetectRestatement(t *testing.T) {
	st := scStore(t)
	ctx := context.Background()
	c := charge(day(2026, 1, 1), nil, 56.19, 59.0)

	first, err := st.PutStandingCharges(ctx, []DailyCharge{c})
	if err != nil || first.Inserted != 1 {
		t.Fatalf("first put: %+v %v", first, err)
	}
	again, err := st.PutStandingCharges(ctx, []DailyCharge{c})
	if err != nil || again.Unchanged != 1 {
		t.Fatalf("re-put must be Unchanged, got %+v %v", again, err)
	}

	c.IncVATPence = 56.19 // the zero rate applied retrospectively
	third, err := st.PutStandingCharges(ctx, []DailyCharge{c})
	if err != nil || third.Restated != 1 {
		t.Fatalf("changed value must be Restated, got %+v %v", third, err)
	}
}

// A charge that STARTED before the window must still be returned: the current
// standing charge typically began long before any month being billed, so a
// containment query would find nothing in the common case.
func TestStandingChargesOverlapRatherThanContain(t *testing.T) {
	st := scStore(t)
	ctx := context.Background()
	if _, err := st.PutStandingCharges(ctx, []DailyCharge{
		charge(day(2024, 1, 1), nil, 56.19, 59.0),
	}); err != nil {
		t.Fatal(err)
	}

	got, err := st.StandingCharges(ctx, "E-1R-AGILE-24-10-01-A", day(2026, 9, 1), day(2026, 10, 1))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("a charge starting two years earlier must cover this window; got %d", len(got))
	}
}

// ---------------------------------------------------------------------------
// Schedule: charging a window
// ---------------------------------------------------------------------------

// The headline case: a window spanning the VAT change is charged at each rate for
// the days it actually applied to.
func TestScheduleChargesEachRateForItsOwnDays(t *testing.T) {
	boundary := day(2026, 10, 1)
	s := NewSchedule([]DailyCharge{
		charge(day(2026, 1, 1), ptr(boundary), 56.19, 59.0), // 5%
		charge(boundary, nil, 56.19, 56.19),                 // zero-rated
	})

	got, ok := s.ChargeOver(day(2026, 9, 29), day(2026, 10, 4))
	if !ok {
		t.Fatal("window is fully covered but ChargeOver said no")
	}
	want := 2*0.59 + 3*0.5619
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("charge £%.6f, want £%.6f", got, want)
	}
}

// A gap yields "I don't know", not a partial total. A partial total looks like a
// correct but suspiciously cheap bill, which is the failure this layer exists to
// prevent.
func TestScheduleRefusesAWindowItDoesNotFullyCover(t *testing.T) {
	s := NewSchedule([]DailyCharge{
		charge(day(2026, 1, 1), ptr(day(2026, 6, 1)), 56.19, 59.0),
	})
	if _, ok := s.ChargeOver(day(2026, 5, 30), day(2026, 6, 3)); ok {
		t.Error("window runs past the last charge; ChargeOver must refuse it")
	}
	if _, ok := s.ChargeOver(day(2025, 12, 30), day(2026, 1, 3)); ok {
		t.Error("window starts before the first charge; ChargeOver must refuse it")
	}
}

// Elapsed time, not calendar days. A window spanning the autumn changeover lasted
// 25 hours, and is charged for what it actually lasted.
func TestScheduleChargesRealElapsedTimeAcrossDST(t *testing.T) {
	loc, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Fatal(err)
	}
	s := NewSchedule([]DailyCharge{charge(day(2020, 1, 1), nil, 56.19, 59.0)})

	// 25 October 2026: the clocks go back, so this local day is 25 hours.
	from := time.Date(2026, 10, 25, 0, 0, 0, 0, loc)
	to := time.Date(2026, 10, 26, 0, 0, 0, 0, loc)

	got, ok := s.ChargeOver(from.UTC(), to.UTC())
	if !ok {
		t.Fatal("not covered")
	}
	if want := (25.0 / 24.0) * 0.59; math.Abs(got-want) > 1e-9 {
		t.Errorf("charge £%.6f, want £%.6f (25 hours, not 24)", got, want)
	}
}

func TestSchedulePencePerDayAtIsVATInclusive(t *testing.T) {
	boundary := day(2026, 10, 1)
	s := NewSchedule([]DailyCharge{
		charge(day(2026, 1, 1), ptr(boundary), 56.19, 59.0),
		charge(boundary, nil, 56.19, 56.19),
	})
	if v, ok := s.PencePerDayAt(day(2026, 9, 30)); !ok || v != 59.0 {
		t.Errorf("before the change: %v %v, want 59", v, ok)
	}
	if v, ok := s.PencePerDayAt(boundary); !ok || v != 56.19 {
		t.Errorf("at the boundary: %v %v, want 56.19 (half-open, later block wins)", v, ok)
	}
	if _, ok := s.PencePerDayAt(day(2025, 1, 1)); ok {
		t.Error("before any charge, PencePerDayAt must say it does not know")
	}
}

// ---------------------------------------------------------------------------
// Validation
// ---------------------------------------------------------------------------

// The cross-check that demotes config's vat_rate to an expectation: a standing
// charge whose inc/exc disagrees with config is STORED and flagged, never
// rejected — same reasoning as the unit-price gate.
func TestStandingChargeVATMismatchWarnsAndNamesTheImpliedRate(t *testing.T) {
	five := 0.05
	res := ValidateStandingCharges(
		[]DailyCharge{charge(day(2026, 10, 1), nil, 56.19, 56.19)}, // zero-rated
		ValidateOptions{ExpectVATRate: &five},                      // config still says 5%
	)

	if len(res.Rejected) != 0 {
		t.Fatalf("a VAT change must never cost us the standing charge: %+v", res.Rejected)
	}
	if len(res.Accepted) != 1 {
		t.Fatalf("accepted %d of 1", len(res.Accepted))
	}
	if len(res.Warnings) != 1 || res.Warnings[0].Kind != WarnVATMismatch {
		t.Fatalf("want one vat_mismatch warning, got %+v", res.Warnings)
	}
	got := res.Warnings[0].ImpliedVATRate
	if got == nil || math.Abs(*got) > 1e-9 {
		t.Errorf("implied VAT %v, want 0 — the number config should be set to", got)
	}
}

func TestStandingChargeGateARejects(t *testing.T) {
	for _, tc := range []struct {
		name   string
		charge DailyCharge
		reason RejectReason
	}{
		{"non-finite", charge(day(2026, 1, 1), nil, math.Inf(1), 1), ReasonPriceNotFinite},
		{"inverted interval", charge(day(2026, 6, 1), ptr(day(2026, 1, 1)), 56.19, 59.0), ReasonValidToNotAfter},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := ValidateStandingCharges([]DailyCharge{tc.charge}, ValidateOptions{})
			if len(res.Rejected) != 1 {
				t.Fatalf("want 1 rejection, got %d", len(res.Rejected))
			}
			if res.Rejected[0].Reason != tc.reason {
				t.Errorf("reason %q, want %q", res.Rejected[0].Reason, tc.reason)
			}
			if len(res.Accepted) != 0 {
				t.Error("a rejected charge must not also be accepted")
			}
		})
	}
}

// A charge for a tariff we did not ask for is refused: it would otherwise be
// billed under the code we requested.
func TestStandingChargeWrongTariffIsRejected(t *testing.T) {
	c := charge(day(2026, 1, 1), nil, 56.19, 59.0)
	c.TariffCode = "E-1R-GO-24-10-01-A"
	res := ValidateStandingCharges([]DailyCharge{c},
		ValidateOptions{TariffCode: "E-1R-AGILE-24-10-01-A"})
	if len(res.Rejected) != 1 || res.Rejected[0].Reason != ReasonTariffMismatch {
		t.Fatalf("want a tariff mismatch rejection, got %+v", res.Rejected)
	}
}
