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

	got, ok := s.ChargeOver(day(2026, 9, 29), day(2026, 10, 4), time.UTC)
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
	if _, ok := s.ChargeOver(day(2026, 5, 30), day(2026, 6, 3), time.UTC); ok {
		t.Error("window runs past the last charge; ChargeOver must refuse it")
	}
	if _, ok := s.ChargeOver(day(2025, 12, 30), day(2026, 1, 3), time.UTC); ok {
		t.Error("window starts before the first charge; ChargeOver must refuse it")
	}
}

// ---------------------------------------------------------------------------
// Clock-change days (issue #56).
//
// A standing charge is a price per CALENDAR DAY of supply. Octopus bills
// 25 October 2026 — a 25-hour local day — at exactly one day's charge, and
// 29 March 2026 — 23 hours — at one day's charge too. This test used to pin the
// opposite (25/24 of a day, "charged for what it actually lasted"), and that is
// how prod came to bill 25 Oct at £0.6163 against the supplier's £0.5916.
//
// Part days stay pro rata, but against the length of THE DAY THEY ARE PART OF:
// twelve hours of a 25-hour day is 12/25 of a day's charge, not 12/24.
// ---------------------------------------------------------------------------

func londonLoc(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

func TestScheduleChargesCalendarDaysAcrossDST(t *testing.T) {
	loc := londonLoc(t)
	// 59p/day inc VAT throughout, so every expectation is "days × £0.59".
	s := NewSchedule([]DailyCharge{charge(day(2020, 1, 1), nil, 56.19, 59.0)})
	at := func(y int, m time.Month, d, h int) time.Time { return time.Date(y, m, d, h, 0, 0, 0, loc) }

	tests := []struct {
		name     string
		from, to time.Time
		days     float64
	}{
		// THE prod case: the whole of the 25-hour day is one day, not 25/24.
		{"25 Oct 2026 whole (25h)", at(2026, 10, 25, 0), at(2026, 10, 26, 0), 1.0},
		// Its mirror image, which under-billed by the same 1/24 of a day.
		{"29 Mar 2026 whole (23h)", at(2026, 3, 29, 0), at(2026, 3, 30, 0), 1.0},
		// First twelve elapsed hours of the long day: 12/25, not 12/24.
		{"25 Oct first 12 elapsed hours", at(2026, 10, 25, 0), at(2026, 10, 25, 0).Add(12 * time.Hour), 12.0 / 25.0},
		// Twenty-four elapsed hours is NOT the whole day: an hour of it is still to come.
		{"25 Oct 00:00 local to 23:00Z", at(2026, 10, 25, 0), time.Date(2026, 10, 25, 23, 0, 0, 0, time.UTC), 24.0 / 25.0},
		// Whole days spanning the change are whole: 2, not 49/24.
		{"24-26 Oct whole days", at(2026, 10, 24, 0), at(2026, 10, 26, 0), 2.0},
		// Regression guard: an ordinary day's half is still half.
		{"26 Oct 00:00-12:00 (ordinary day)", at(2026, 10, 26, 0), at(2026, 10, 26, 12), 0.5},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// UTC instants, exactly as archivedStandingCharge passes them: the
			// calendar comes from loc, not from the arguments' own Location.
			got, ok := s.ChargeOver(tc.from.UTC(), tc.to.UTC(), loc)
			if !ok {
				t.Fatal("not covered")
			}
			if want := tc.days * 0.59; math.Abs(got-want) > 1e-9 {
				t.Errorf("charge £%.6f, want £%.6f (%.6f days at £0.59)", got, want, tc.days)
			}
		})
	}
}

// A standing-charge change landing INSIDE a clock-change day: each side is
// charged its share of that 25-hour day at its own rate, and the shares still
// add up to exactly one day. The cursor loop calls Days once per charge, so this
// is what proves the per-segment pieces are measured against the day they sit
// in rather than against 24 hours each.
func TestScheduleSplitsAClockChangeDayAtARateChange(t *testing.T) {
	loc := londonLoc(t)
	dayStart := time.Date(2026, 10, 25, 0, 0, 0, 0, loc)
	// Five elapsed hours in: 04:00 GMT, after the repeated 01:00 hour.
	change := dayStart.Add(5 * time.Hour)
	s := NewSchedule([]DailyCharge{
		charge(day(2020, 1, 1), ptr(change.UTC()), 50, 50.0), // 50p/day
		charge(change.UTC(), nil, 75, 75.0),                  // 75p/day
	})

	got, ok := s.ChargeOver(dayStart.UTC(), dayStart.AddDate(0, 0, 1).UTC(), loc)
	if !ok {
		t.Fatal("not covered")
	}
	want := (5.0/25.0)*0.50 + (20.0/25.0)*0.75
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("charge £%.6f, want £%.6f (5/25 at 50p + 20/25 at 75p)", got, want)
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
	// And the rate it was checked against, so an alert can say what config says
	// without reaching for some other field (#52).
	if exp := res.Warnings[0].ExpectedVATRate; exp == nil || *exp != 0.05 {
		t.Errorf("expected VAT %v, want 0.05 — the rate this charge was checked against", exp)
	}
}

// Unit prices carry the same pair, resolved per slot.
func TestVATMismatchWarningCarriesTheRateTheSlotWasCheckedAgainst(t *testing.T) {
	from := time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC)
	to := from.Add(30 * time.Minute)
	slot := Slot{
		TariffCode: "E-1R-AGILE-24-10-01-N", ValidFrom: from, ValidTo: &to,
		ExcVATPence: 20, IncVATPence: 20, // zero-rated
	}
	res := Validate([]Slot{slot}, ValidateOptions{
		ExpectVATRateAt: func(time.Time) (float64, bool) { return 0.05, true },
	})
	if len(res.Warnings) != 1 || res.Warnings[0].Kind != WarnVATMismatch {
		t.Fatalf("want one vat_mismatch warning, got %+v", res.Warnings)
	}
	w := res.Warnings[0]
	if w.ImpliedVATRate == nil || *w.ImpliedVATRate != 0 {
		t.Errorf("implied VAT %v, want 0", w.ImpliedVATRate)
	}
	if w.ExpectedVATRate == nil || *w.ExpectedVATRate != 0.05 {
		t.Errorf("expected VAT %v, want 0.05", w.ExpectedVATRate)
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

// ---------------------------------------------------------------------------
// first_seen_at: the fact the sweep was destroying
// ---------------------------------------------------------------------------

// retrieved_at advances on every confirmation; first_seen_at must not move once
// written. This is the pair that lets "when did the supplier publish this?" and
// "when did we last confirm it?" both be answerable — the first sweep overwrote
// 716 rows' worth of the former before the column existed.
func TestFirstSeenAtIsImmutableWhileRetrievedAtAdvances(t *testing.T) {
	st := scStore(t)
	ctx := context.Background()
	from := day(2026, 3, 1)
	end := from.Add(SlotLength)

	first := Slot{
		TariffCode: "E-1R-AGILE-24-10-01-A", ValidFrom: from, ValidTo: &end,
		ExcVATPence: 20, IncVATPence: 21,
		RetrievedAt: time.Date(2026, 2, 28, 15, 55, 0, 0, time.UTC), // publication
	}
	if _, err := st.Put(ctx, []Slot{first}); err != nil {
		t.Fatal(err)
	}

	// A sweep re-reads it a fortnight later, unchanged.
	sweep := first
	sweep.RetrievedAt = time.Date(2026, 3, 14, 2, 0, 0, 0, time.UTC)
	res, err := st.Put(ctx, []Slot{sweep})
	if err != nil || res.Unchanged != 1 {
		t.Fatalf("sweep should be Unchanged: %+v %v", res, err)
	}

	var firstSeen, retrieved string
	if err := st.db.DB().QueryRowContext(ctx,
		`SELECT first_seen_at, retrieved_at FROM unit_price WHERE valid_from = ?`,
		from.Format(timeLayout)).Scan(&firstSeen, &retrieved); err != nil {
		t.Fatal(err)
	}

	if want := "2026-02-28T15:55:00Z"; firstSeen != want {
		t.Errorf("first_seen_at = %q, want %q — the sweep must not move it", firstSeen, want)
	}
	if want := "2026-03-14T02:00:00Z"; retrieved != want {
		t.Errorf("retrieved_at = %q, want %q — it means LAST CONFIRMED", retrieved, want)
	}
}

// A restatement changes the value and moves retrieved_at, but the row is still
// the same row: we first saw a price for that half hour when we first saw it.
func TestFirstSeenAtSurvivesARestatement(t *testing.T) {
	st := scStore(t)
	ctx := context.Background()
	from := day(2026, 3, 2)
	end := from.Add(SlotLength)

	s := Slot{
		TariffCode: "E-1R-AGILE-24-10-01-A", ValidFrom: from, ValidTo: &end,
		ExcVATPence: 20, IncVATPence: 21,
		RetrievedAt: time.Date(2026, 3, 1, 15, 55, 0, 0, time.UTC),
	}
	if _, err := st.Put(ctx, []Slot{s}); err != nil {
		t.Fatal(err)
	}
	s.IncVATPence = 25
	s.RetrievedAt = time.Date(2026, 3, 2, 9, 10, 0, 0, time.UTC)
	res, err := st.Put(ctx, []Slot{s})
	if err != nil || res.Restated != 1 {
		t.Fatalf("want a restatement: %+v %v", res, err)
	}

	var firstSeen string
	if err := st.db.DB().QueryRowContext(ctx,
		`SELECT first_seen_at FROM unit_price WHERE valid_from = ?`,
		from.Format(timeLayout)).Scan(&firstSeen); err != nil {
		t.Fatal(err)
	}
	if want := "2026-03-01T15:55:00Z"; firstSeen != want {
		t.Errorf("first_seen_at = %q, want %q", firstSeen, want)
	}
}

// Standing charges get the column too, so the storage path stays one path.
func TestStandingChargesAlsoRecordFirstSeen(t *testing.T) {
	st := scStore(t)
	ctx := context.Background()
	c := charge(day(2026, 1, 1), nil, 56.19, 59.0)
	c.RetrievedAt = time.Date(2026, 1, 1, 8, 0, 0, 0, time.UTC)
	if _, err := st.PutStandingCharges(ctx, []DailyCharge{c}); err != nil {
		t.Fatal(err)
	}
	var firstSeen string
	if err := st.db.DB().QueryRowContext(ctx,
		`SELECT first_seen_at FROM standing_charge WHERE valid_from = ?`,
		c.ValidFrom.Format(timeLayout)).Scan(&firstSeen); err != nil {
		t.Fatal(err)
	}
	if want := "2026-01-01T08:00:00Z"; firstSeen != want {
		t.Errorf("first_seen_at = %q, want %q", firstSeen, want)
	}
}
