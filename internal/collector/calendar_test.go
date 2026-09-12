package collector

import (
	"context"
	"testing"
	"time"
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
func TestTomorrowRollsOverCorrectly(t *testing.T) {
	loc := london(t)

	for _, tc := range []struct {
		name     string
		now      time.Time
		wantDate string
	}{
		{
			name: "28 February in a LEAP year: tomorrow is the 29th",
			now:  time.Date(2028, 2, 28, 16, 10, 0, 0, loc), wantDate: "2028-02-29",
		},
		{
			name: "28 February in a NON-leap year: tomorrow is 1 March",
			now:  time.Date(2027, 2, 28, 16, 10, 0, 0, loc), wantDate: "2027-03-01",
		},
		{
			name: "29 February: tomorrow is 1 March",
			now:  time.Date(2028, 2, 29, 16, 10, 0, 0, loc), wantDate: "2028-03-01",
		},
		{
			name: "31 December: tomorrow is in the next year",
			now:  time.Date(2026, 12, 31, 16, 10, 0, 0, loc), wantDate: "2027-01-01",
		},
		{
			name: "30 April: tomorrow is 1 May",
			now:  time.Date(2026, 4, 30, 16, 10, 0, 0, loc), wantDate: "2026-05-01",
		},
		{
			// The day before the clocks go back. Tomorrow is the 25-hour day, and
			// the window for it must be 25 hours wide.
			name: "the day before autumn back",
			now:  time.Date(2026, 10, 24, 16, 10, 0, 0, loc), wantDate: "2026-10-25",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tomorrow := tc.now.AddDate(0, 0, 1)
			start, end := localDayWindow(tomorrow, loc)
			if got := start.In(loc).Format("2006-01-02"); got != tc.wantDate {
				t.Errorf("tomorrow = %s, want %s", got, tc.wantDate)
			}
			if got := start.In(loc).Format("15:04"); got != "00:00" {
				t.Errorf("window starts at %s local, want midnight", got)
			}
			// The window length is the zone's business, not 24h.
			hours := end.Sub(start).Hours()
			if hours != 23 && hours != 24 && hours != 25 {
				t.Errorf("window is %v hours, want 23, 24 or 25", hours)
			}
		})
	}
}

// A 25-hour day must produce a 25-hour window, and a 23-hour day a 23-hour one.
// This is the assertion that fails the moment somebody writes Add(24*time.Hour).
func TestLocalDayWindowLengthFollowsTheZone(t *testing.T) {
	loc := london(t)

	for _, tc := range []struct {
		name  string
		day   time.Time
		hours float64
	}{
		{name: "2026 spring forward", day: time.Date(2026, 3, 29, 12, 0, 0, 0, loc), hours: 23},
		{name: "2026 autumn back", day: time.Date(2026, 10, 25, 12, 0, 0, 0, loc), hours: 25},
		{name: "2027 spring forward", day: time.Date(2027, 3, 28, 12, 0, 0, 0, loc), hours: 23},
		{name: "2027 autumn back", day: time.Date(2027, 10, 31, 12, 0, 0, 0, loc), hours: 25},
		{name: "2028 spring forward, leap year", day: time.Date(2028, 3, 26, 12, 0, 0, 0, loc), hours: 23},
		{name: "2028 autumn back, leap year", day: time.Date(2028, 10, 29, 12, 0, 0, 0, loc), hours: 25},
		{name: "leap day is ordinary", day: time.Date(2028, 2, 29, 12, 0, 0, 0, loc), hours: 24},
		{name: "an ordinary summer day", day: time.Date(2026, 6, 15, 12, 0, 0, 0, loc), hours: 24},
	} {
		t.Run(tc.name, func(t *testing.T) {
			start, end := localDayWindow(tc.day, loc)
			if got := end.Sub(start).Hours(); got != tc.hours {
				t.Errorf("window = %v hours, want %v", got, tc.hours)
			}
		})
	}
}

// A sweep of N days across 29 February must reach back N CALENDAR days, not
// N*24h — otherwise in a leap year it quietly covers one day less than asked.
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
	start, end := localDayWindow(time.Date(2028, 2, 29, 12, 0, 0, 0, loc), loc)
	held, err := h.store.Range(context.Background(), testTariffCode, start, end)
	if err != nil {
		t.Fatal(err)
	}
	if len(held) != 48 {
		t.Errorf("the archive holds %d slots for 2028-02-29, want 48", len(held))
	}

	// And the sweep reached the full seven calendar days back.
	earliest, _ := localDayWindow(now.AddDate(0, 0, -7), loc)
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
	_, tomorrowEnd := localDayWindow(now.AddDate(0, 0, 1), loc)

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
	_, tomorrowEnd := localDayWindow(now.AddDate(0, 0, 1), loc)

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
