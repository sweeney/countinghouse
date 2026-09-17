package collector

import (
	"context"
	"testing"
	"time"

	"github.com/sweeney/countinghouse/internal/prices"
)

// ---------------------------------------------------------------------------
// Calendar arithmetic in the collector.
//
// Leap years do not change how many slots a day holds — UK clock changes are
// always in March and October, so 29 February is an ordinary 48-slot day. What
// leap years DO change is date arithmetic: what "tomorrow" is on 28 February, and
// how far back a sweep of N days reaches. Both use AddDate and time.Date, which
// are correct by construction; these tests pin that, because adding
// 24*time.Hour looks like a harmless simplification and is wrong four days a year.
// ---------------------------------------------------------------------------

// "Tomorrow" is what the collector assesses for completeness after a
// publication, so getting it wrong on 28 February would mean checking the wrong
// day — and, in a leap year, never checking 29 February at all.
// The two pure-function tests that used to live here — day rollover across leap years
// and year boundaries, and local-day length across every DST changeover to 2028 — moved
// out with the function. `localDayWindow` was a verbatim duplicate of the exported,
// already-tested `prices.LocalDayWindow`, doc comment and all, and two copies of a
// DST-critical function is one more than wanted. The coverage now lives beside the one
// implementation, in internal/prices/calendar_test.go.
//
// What remains here is what belongs here: the COLLECTOR's behaviour across those same
// calendar edges, which is a different question from whether the helper is right.

func TestCatchUpSpansALeapDay(t *testing.T) {
	loc := london(t)
	// Sweeping 7 days back from 3 March 2028 must reach 25 February, crossing the
	// 29th. A 24h-based calculation lands a day late.
	now := time.Date(2028, 3, 3, 16, 10, 0, 0, loc)

	f := newFakeFetcher(ts(t, "2028-01-01T00:00:00Z"), ts(t, "2028-03-04T00:00:00Z"))
	h := newHarness(t, now.UTC(), f)

	res, err := h.c.CatchUp(context.Background(), 7)
	if err != nil {
		t.Fatalf("CatchUp: %v", err)
	}
	if res.Stored.Inserted == 0 {
		t.Fatal("the sweep stored nothing")
	}

	// The leap day itself must be in the archive, all 48 slots of it.
	start, end := prices.LocalDayWindow(time.Date(2028, 2, 29, 12, 0, 0, 0, loc), loc)
	held, err := h.store.Range(context.Background(), testTariffCode, start, end)
	if err != nil {
		t.Fatal(err)
	}
	if len(held) != 48 {
		t.Errorf("the archive holds %d slots for 2028-02-29, want 48", len(held))
	}

	// And the sweep reached the full seven calendar days back.
	earliest, _ := prices.LocalDayWindow(now.AddDate(0, 0, -7), loc)
	if got := earliest.In(loc).Format("2006-01-02"); got != "2028-02-25" {
		t.Errorf("seven days before 2028-03-03 is %s, want 2028-02-25", got)
	}
	oldest, err := h.store.Range(context.Background(), testTariffCode, earliest, earliest.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(oldest) == 0 {
		t.Error("the sweep did not reach the seventh day back; a 24h-based span would fall short across a leap day")
	}
}

// A sync on the leap day itself behaves like any other day.
func TestSyncOnALeapDay(t *testing.T) {
	loc := london(t)
	now := time.Date(2028, 2, 29, 16, 10, 0, 0, loc)
	// Published to the end of local 1 March, so both the leap day and tomorrow
	// are complete.
	_, tomorrowEnd := prices.LocalDayWindow(now.AddDate(0, 0, 1), loc)

	f := newFakeFetcher(ts(t, "2028-02-27T00:00:00Z"), tomorrowEnd)
	h := newHarness(t, now.UTC(), f)

	res, err := h.c.Sync(context.Background())
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if !res.TomorrowComplete {
		t.Errorf("1 March should be complete: %+v", res)
	}
	if st := h.c.Status(); !st.CompleteTo.Equal(tomorrowEnd) {
		t.Errorf("CompleteTo = %s, want %s", st.CompleteTo, tomorrowEnd)
	}
	if kinds := h.noti.kinds(); len(kinds) != 0 {
		t.Errorf("an ordinary leap day raised alerts: %v", kinds)
	}
}

// Crossing the year boundary must not confuse today/tomorrow.
func TestSyncAcrossTheYearBoundary(t *testing.T) {
	loc := london(t)
	now := time.Date(2026, 12, 31, 16, 10, 0, 0, loc)
	_, tomorrowEnd := prices.LocalDayWindow(now.AddDate(0, 0, 1), loc)

	f := newFakeFetcher(ts(t, "2026-12-29T00:00:00Z"), tomorrowEnd)
	h := newHarness(t, now.UTC(), f)

	res, err := h.c.Sync(context.Background())
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if !res.TomorrowComplete {
		t.Errorf("1 January should be complete: %+v", res)
	}
	if got := tomorrowEnd.In(loc).Format("2006-01-02"); got != "2027-01-02" {
		t.Errorf("tomorrow's window ends %s, want 2027-01-02", got)
	}
	if kinds := h.noti.kinds(); len(kinds) != 0 {
		t.Errorf("the year boundary raised alerts: %v", kinds)
	}
}
