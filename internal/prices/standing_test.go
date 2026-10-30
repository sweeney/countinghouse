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

// ---------------------------------------------------------------------------
// ChargeOverAtVAT: the supplier's inc-VAT figure only where it matches the day
// ---------------------------------------------------------------------------

// #51: one charge, open-ended from 2024, restated in place to its zero-rated
// figure. Charged at 5%, it must be grossed up from ex-VAT, and say so.
func TestChargeOverAtVATRegrossesABackdatedZeroRate(t *testing.T) {
	s := NewSchedule([]DailyCharge{charge(day(2024, 9, 30), nil, 59.1606, 59.1606)})

	got, regrossed, ok := s.ChargeOverAtVAT(day(2026, 9, 21), day(2026, 9, 22), 0.05, time.UTC)
	if !ok {
		t.Fatal("window is fully covered but ChargeOverAtVAT said no")
	}
	if want := 0.591606 * 1.05; math.Abs(got-want) > 1e-9 {
		t.Errorf("charge £%.6f, want £%.6f", got, want)
	}
	if !regrossed {
		t.Error("regrossed = false; the inc-VAT figure was not used and the caller must know")
	}
}

// Where the supplier's figure implies the rate asked for, it is used as-is.
func TestChargeOverAtVATUsesTheSupplierFigureWhenItAgrees(t *testing.T) {
	s := NewSchedule([]DailyCharge{charge(day(2024, 9, 30), nil, 59.1606, 59.1606)})

	got, regrossed, ok := s.ChargeOverAtVAT(day(2026, 10, 2), day(2026, 10, 4), 0, time.UTC)
	if !ok {
		t.Fatal("not covered")
	}
	if want := 2 * 0.591606; math.Abs(got-want) > 1e-9 {
		t.Errorf("charge £%.6f, want £%.6f", got, want)
	}
	if regrossed {
		t.Error("regrossed = true for a figure that implies exactly the rate asked for")
	}
}

// Each charge in the window is judged on its own: only the one that disagrees
// is grossed up.
func TestChargeOverAtVATJudgesEachChargeSeparately(t *testing.T) {
	boundary := day(2026, 6, 1)
	s := NewSchedule([]DailyCharge{
		charge(day(2026, 1, 1), ptr(boundary), 50, 52.5), // 5%, agrees
		charge(boundary, nil, 56.19, 56.19),              // 0%, does not
	})

	got, regrossed, ok := s.ChargeOverAtVAT(day(2026, 5, 31), day(2026, 6, 2), 0.05, time.UTC)
	if !ok {
		t.Fatal("not covered")
	}
	if want := 0.525 + 0.5619*1.05; math.Abs(got-want) > 1e-9 {
		t.Errorf("charge £%.6f, want £%.6f", got, want)
	}
	if !regrossed {
		t.Error("regrossed = false, but the second charge was grossed up")
	}
}

// ChargeOverAtVAT refuses a window it does not fully cover, exactly as
// ChargeOver does.
func TestChargeOverAtVATRefusesAGap(t *testing.T) {
	s := NewSchedule([]DailyCharge{charge(day(2026, 1, 1), ptr(day(2026, 6, 1)), 56.19, 59.0)})
	if _, _, ok := s.ChargeOverAtVAT(day(2026, 5, 30), day(2026, 6, 3), 0.05, time.UTC); ok {
		t.Error("window runs past the last charge; ChargeOverAtVAT must refuse it")
	}
}

func TestImpliesVATRate(t *testing.T) {
	for _, tc := range []struct {
		name     string
		exc, inc float64
		rate     float64
		want     bool
	}{
		{"exact 5%", 59.1606, 62.11863, 0.05, true},
		{"supplier rounding of 5%", 59.1606, 62.1186, 0.05, true},
		{"zero rate", 59.1606, 59.1606, 0, true},
		{"zero-rated figure, 5% expected", 59.1606, 59.1606, 0.05, false},
		{"5% figure, zero expected", 59.1606, 62.11863, 0, false},
		{"20% figure, 5% expected", 50, 60, 0.05, false},
		{"no ex-VAT figure is taken at its word", 0, 59, 0.05, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := charge(day(2026, 1, 1), nil, tc.exc, tc.inc)
			if got := c.ImpliesVATRate(tc.rate); got != tc.want {
				t.Errorf("ImpliesVATRate(%v) for exc %v inc %v = %v, want %v",
					tc.rate, tc.exc, tc.inc, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// History: each day at the figure the supplier was publishing for it
// ---------------------------------------------------------------------------

// vatOnly is a restatement that moved only the inc-VAT figure: a VAT change.
func vatOnly(t *testing.T, c DailyCharge, oldInc, newInc float64, prev, detected string) Restatement {
	return Restatement{
		Key:            c.row().Key(),
		OldExcVATPence: c.ExcVATPence, OldIncVATPence: oldInc,
		NewExcVATPence: c.ExcVATPence, NewIncVATPence: newInc,
		PreviousRetrievedAt: at(t, prev), DetectedAt: at(t, detected),
	}
}

// #51's charge as production holds it: first seen at 5% on 17 Sep, restated in
// place to 0% by the sweep at 01:03Z on 1 Oct, the one before having run a day
// earlier.
func productionHistory(t *testing.T) ChargeHistory {
	c := charge(at(t, "2024-09-30T23:00:00Z"), nil, 59.1606, 59.1606)
	return ChargeHistory{
		Charge:       c,
		FirstSeenAt:  at(t, "2026-09-17T01:03:00Z"),
		Restatements: []Restatement{vatOnly(t, c, 62.1186, 59.1606, "2026-09-30T01:03:00Z", "2026-10-01T01:03:00Z")},
	}
}

func chargeAt(t *testing.T, s Schedule, from, to string, vat float64) (float64, bool) {
	t.Helper()
	got, regrossed, ok := s.ChargeOverAtVAT(at(t, from), at(t, to), vat, londonLoc(t))
	if !ok {
		t.Fatalf("%s..%s is covered but ChargeOverAtVAT said no", from, to)
	}
	return got, regrossed
}

func wantPence(t *testing.T, got float64, wantPencePerDay, days float64) {
	t.Helper()
	if want := wantPencePerDay * days / 100; math.Abs(got-want) > 1e-9 {
		t.Errorf("charge £%.6f, want £%.6f (%.4fp/day for %.4f days)", got, want, wantPencePerDay, days)
	}
}

// A day the archive watched go by is billed at what the supplier published for
// it then, not at the zero-rated figure the row was later restated to — and not
// at whatever the agreement says either, which here is wrong on purpose.
func TestHistoryBillsAWitnessedDayAtItsOwnFigure(t *testing.T) {
	s := NewScheduleFromHistory([]ChargeHistory{productionHistory(t)})
	for _, vat := range []float64{0.05, 0} {
		got, regrossed := chargeAt(t, s, "2026-09-20T23:00:00Z", "2026-09-21T23:00:00Z", vat)
		wantPence(t, got, 62.1186, 1)
		if regrossed {
			t.Errorf("at an agreement of %v: regrossed a day the supplier's figure was witnessed for", vat)
		}
	}
}

// After the restatement was seen, the new figure, again whatever the agreement
// says: a configuration that never authored the split cannot move the bill.
func TestHistoryBillsAfterTheRestatementAtTheNewFigure(t *testing.T) {
	s := NewScheduleFromHistory([]ChargeHistory{productionHistory(t)})
	got, regrossed := chargeAt(t, s, "2026-10-05T23:00:00Z", "2026-10-06T23:00:00Z", 0.05)
	wantPence(t, got, 59.1606, 1)
	if regrossed {
		t.Error("regrossed a witnessed day")
	}
}

// Between the last sighting of the old figure and the first of the new, the
// agreement picks whichever of the two it agrees with.
func TestHistoryLetsTheAgreementPlaceTheChangeBetweenSweeps(t *testing.T) {
	s := NewScheduleFromHistory([]ChargeHistory{productionHistory(t)})

	before, _ := chargeAt(t, s, "2026-09-30T01:03:00Z", "2026-09-30T23:00:00Z", 0.05)
	wantPence(t, before, 62.1186, (23-(1+3.0/60))/24)

	after, _ := chargeAt(t, s, "2026-09-30T23:00:00Z", "2026-10-01T01:03:00Z", 0)
	wantPence(t, after, 59.1606, (2+3.0/60)/24)

	// An agreement neither figure matches: ex-VAT at its rate, flagged.
	odd, regrossed := chargeAt(t, s, "2026-09-30T23:00:00Z", "2026-10-01T01:03:00Z", 0.2)
	wantPence(t, odd, 59.1606*1.2, (2+3.0/60)/24)
	if !regrossed {
		t.Error("regrossed = false for a figure neither side of the restatement implies")
	}
}

// The zero rate ends on 31 Mar 2027. Suppose configuration never authors that
// split, so the 0% agreement runs on, and Octopus restates the charge back to
// 5% in place. Days after the sweep saw it are billed at 5%: the supplier's
// figure, not the forgotten agreement's under-charge.
func TestHistoryBillsTheReturnTo5PercentOverAForgottenAgreement(t *testing.T) {
	h := productionHistory(t)
	h.Restatements = append(h.Restatements,
		vatOnly(t, h.Charge, 59.1606, 62.1186, "2027-03-31T01:03:00Z", "2027-04-01T01:03:00Z"))
	h.Charge.IncVATPence = 62.1186
	s := NewScheduleFromHistory([]ChargeHistory{h})

	got, regrossed := chargeAt(t, s, "2027-04-04T23:00:00Z", "2027-04-05T23:00:00Z", 0)
	wantPence(t, got, 62.1186, 1)
	if regrossed {
		t.Error("regrossed a witnessed day")
	}
	// The zero-rated months in between are still zero-rated.
	mid, _ := chargeAt(t, s, "2026-12-01T00:00:00Z", "2026-12-02T00:00:00Z", 0)
	wantPence(t, mid, 59.1606, 1)
	// And September is still 5%, two restatements later.
	sep, _ := chargeAt(t, s, "2026-09-20T23:00:00Z", "2026-09-21T23:00:00Z", 0.05)
	wantPence(t, sep, 62.1186, 1)
}

// Before the archive first saw a charge, nobody saw what the supplier charged.
// The figure held is a later one carried back, believed where it agrees with the
// agreement and grossed up from ex-VAT where it does not.
func TestHistoryJudgesDaysBeforeTheFirstSightingByTheAgreement(t *testing.T) {
	s := NewScheduleFromHistory([]ChargeHistory{productionHistory(t)})
	got, regrossed := chargeAt(t, s, "2025-06-01T00:00:00Z", "2025-06-02T00:00:00Z", 0.05)
	wantPence(t, got, 62.1186, 1)
	if regrossed {
		t.Error("regrossed a carried-back figure that agrees with the agreement")
	}

	rebuilt := ChargeHistory{Charge: productionHistory(t).Charge, FirstSeenAt: at(t, "2026-10-20T01:03:00Z")}
	s = NewScheduleFromHistory([]ChargeHistory{rebuilt})
	got, regrossed = chargeAt(t, s, "2026-09-20T23:00:00Z", "2026-09-21T23:00:00Z", 0.05)
	wantPence(t, got, 59.1606*1.05, 1)
	if !regrossed {
		t.Error("regrossed = false; a rebuilt archive's zero-rated figure was billed for a 5% day")
	}
}

// A restatement that moves the ex-VAT figure is a correction, the supplier's
// account of every day the charge covers. The superseded figure is not billed
// for any of them; days before the correction was seen are judged as unwitnessed.
func TestHistoryTreatsAnExVATChangeAsACorrection(t *testing.T) {
	c := charge(at(t, "2024-09-30T23:00:00Z"), nil, 60, 63)
	h := ChargeHistory{
		Charge:      c,
		FirstSeenAt: at(t, "2026-09-17T01:03:00Z"),
		Restatements: []Restatement{{
			Key:            c.row().Key(),
			OldExcVATPence: 59.1606, OldIncVATPence: 62.1186,
			NewExcVATPence: 60, NewIncVATPence: 63,
			PreviousRetrievedAt: at(t, "2026-09-24T01:03:00Z"), DetectedAt: at(t, "2026-09-25T01:03:00Z"),
		}},
	}
	s := NewScheduleFromHistory([]ChargeHistory{h})
	got, regrossed := chargeAt(t, s, "2026-09-20T23:00:00Z", "2026-09-21T23:00:00Z", 0.05)
	wantPence(t, got, 63, 1)
	if regrossed {
		t.Error("the corrected figure implies the agreement's 5% and needs no regross")
	}
}

// A row older than the first_seen_at column was seen no later than its earliest
// confirmation on record: the old figure's last one, when it has been restated.
// Nothing before that was witnessed.
func TestHistoryWithoutFirstSeenFallsBackToTheEarliestConfirmation(t *testing.T) {
	h := productionHistory(t)
	h.FirstSeenAt = time.Time{}
	s := NewScheduleFromHistory([]ChargeHistory{h})

	// Before 30 Sep 01:03Z the 5% figure is unwitnessed; a 0% agreement there
	// disbelieves it and regrosses.
	_, regrossed := chargeAt(t, s, "2026-09-20T23:00:00Z", "2026-09-21T23:00:00Z", 0)
	if !regrossed {
		t.Error("a day before any confirmation on record was treated as witnessed")
	}
	// After the restatement was seen, the new figure is witnessed as usual.
	got, regrossed := chargeAt(t, s, "2026-10-05T23:00:00Z", "2026-10-06T23:00:00Z", 0.05)
	wantPence(t, got, 59.1606, 1)
	if regrossed {
		t.Error("regrossed a witnessed day")
	}
}

// The store hands back each charge with its first sighting and its restatement
// log, oldest first, so a bill can be built from them.
func TestStandingChargeHistoriesCarryFirstSeenAndRestatements(t *testing.T) {
	st := scStore(t)
	ctx := context.Background()
	c := charge(at(t, "2024-09-30T23:00:00Z"), nil, 59.1606, 62.1186)
	c.RetrievedAt = at(t, "2026-09-17T01:03:00Z")
	other := charge(at(t, "2020-01-01T00:00:00Z"), ptr(at(t, "2024-09-30T23:00:00Z")), 50, 52.5)
	if _, err := st.PutStandingCharges(ctx, []DailyCharge{c, other}); err != nil {
		t.Fatal(err)
	}
	for _, step := range []struct {
		inc float64
		at  string
	}{{62.1186, "2026-09-30T01:03:00Z"}, {59.1606, "2026-10-01T01:03:00Z"}, {62.1186, "2027-04-01T01:03:00Z"}} {
		c.IncVATPence, c.RetrievedAt = step.inc, at(t, step.at)
		if _, err := st.PutStandingCharges(ctx, []DailyCharge{c}); err != nil {
			t.Fatal(err)
		}
	}

	hs, err := st.StandingChargeHistories(ctx, c.TariffCode, at(t, "2026-09-01T00:00:00Z"), at(t, "2027-05-01T00:00:00Z"))
	if err != nil {
		t.Fatal(err)
	}
	if len(hs) != 1 {
		t.Fatalf("got %d histories, want only the charge overlapping the window", len(hs))
	}
	h := hs[0]
	if !h.FirstSeenAt.Equal(at(t, "2026-09-17T01:03:00Z")) {
		t.Errorf("first seen %s, want 2026-09-17T01:03:00Z", h.FirstSeenAt)
	}
	if len(h.Restatements) != 2 {
		t.Fatalf("got %d restatements, want 2: %+v", len(h.Restatements), h.Restatements)
	}
	if r := h.Restatements[0]; r.NewIncVATPence != 59.1606 || !r.PreviousRetrievedAt.Equal(at(t, "2026-09-30T01:03:00Z")) {
		t.Errorf("first restatement %+v, want the 1 Oct zero rating, oldest first", r)
	}
	if r := h.Restatements[1]; r.NewIncVATPence != 62.1186 || !r.DetectedAt.Equal(at(t, "2027-04-01T01:03:00Z")) {
		t.Errorf("second restatement %+v, want the 1 Apr return to 5%%", r)
	}

	got, _ := chargeAt(t, NewScheduleFromHistory(hs), "2026-09-20T23:00:00Z", "2026-09-21T23:00:00Z", 0)
	wantPence(t, got, 62.1186, 1)
}

// An open-ended charge is judged at the VAT in force when it was retrieved, not
// at its start in 2024. Production's restated charge, after the split is
// authored, must not warn on every daily sync for ever.
func TestStandingChargeVATIsJudgedWhenItWasRetrieved(t *testing.T) {
	c := charge(at(t, "2024-09-30T23:00:00Z"), nil, 59.1606, 59.1606)
	c.RetrievedAt = at(t, "2026-10-26T01:03:00Z")
	cut := at(t, "2026-09-30T23:00:00Z")
	split := func(when time.Time) (float64, bool) {
		if when.Before(cut) {
			return 0.05, true
		}
		return 0, true
	}
	if res := ValidateStandingCharges([]DailyCharge{c}, ValidateOptions{ExpectVATRateAt: split}); len(res.Warnings) != 0 {
		t.Errorf("warned on a zero-rated charge retrieved under a 0%% agreement: %+v", res.Warnings)
	}

	// A charge that ended before it was retrieved is judged at its last instant.
	past := charge(at(t, "2026-01-01T00:00:00Z"), &cut, 59.1606, 59.1606*1.05)
	past.RetrievedAt = c.RetrievedAt
	if res := ValidateStandingCharges([]DailyCharge{past}, ValidateOptions{ExpectVATRateAt: split}); len(res.Warnings) != 0 {
		t.Errorf("warned on a 5%% charge that ended at the cut: %+v", res.Warnings)
	}
}

// Issue #56 on the era-aware path (#55), which is the one /bill actually uses.
// ChargeOverAtVAT cuts a charge into eras and measures each piece separately,
// so it needs the calendar-day rule as much as ChargeOver does: production's
// zero-rated figure, witnessed from 1 Oct, billed for the whole local day of
// 25 Oct 2026 (25 hours) must come to exactly one day's charge.
func TestChargeOverAtVATChargesTheAutumnClockChangeAsOneDay(t *testing.T) {
	loc := londonLoc(t)
	s := NewScheduleFromHistory([]ChargeHistory{productionHistory(t)})
	from := time.Date(2026, 10, 25, 0, 0, 0, 0, loc)
	to := time.Date(2026, 10, 26, 0, 0, 0, 0, loc)

	got, regrossed, ok := s.ChargeOverAtVAT(from.UTC(), to.UTC(), 0, loc)
	if !ok {
		t.Fatal("covered but ChargeOverAtVAT said no")
	}
	if regrossed {
		t.Error("a witnessed day must bill the supplier's figure, not a regrossed one")
	}
	wantPence(t, got, 59.1606, 1.0) // not 25/24
}

// An era boundary INSIDE the 25-hour day, on the path /bill uses (PR #57
// review). Era boundaries are arbitrary sweep instants (FirstSeenAt,
// PreviousRetrievedAt, DetectedAt), so they land mid-day as a matter of course,
// and only calendar.Days' additivity keeps the two pieces of 25 Oct summing to
// one day. A VAT-only restatement detected at 03:00Z splits the local day
// (24 Oct 23:00Z .. 26 Oct 00:00Z) into 4 hours at the old witnessed figure and
// 21 hours at the new one: 4/25 + 21/25, not 4/24 + 21/24.
func TestChargeOverAtVATSplitsTheAutumnClockChangeDayAtAnEra(t *testing.T) {
	loc := londonLoc(t)
	c := charge(at(t, "2024-09-30T23:00:00Z"), nil, 59.1606, 59.1606)
	s := NewScheduleFromHistory([]ChargeHistory{{
		Charge:      c,
		FirstSeenAt: at(t, "2026-09-17T01:03:00Z"),
		// Previous confirmation and detection at the same instant: no "between"
		// era, just witnessed-old then witnessed-new.
		Restatements: []Restatement{vatOnly(t, c, 62.1186, 59.1606, "2026-10-25T03:00:00Z", "2026-10-25T03:00:00Z")},
	}})
	from := time.Date(2026, 10, 25, 0, 0, 0, 0, loc)
	to := time.Date(2026, 10, 26, 0, 0, 0, 0, loc)

	got, regrossed, ok := s.ChargeOverAtVAT(from.UTC(), to.UTC(), 0, loc)
	if !ok {
		t.Fatal("covered but ChargeOverAtVAT said no")
	}
	if regrossed {
		t.Error("both eras are witnessed; nothing should be regrossed")
	}
	want := (4.0/25.0)*62.1186 + (21.0/25.0)*59.1606
	if math.Abs(got-want/100) > 1e-9 {
		t.Errorf("charge £%.6f, want £%.6f (4/25 at 62.1186p + 21/25 at 59.1606p)", got, want/100)
	}
}
