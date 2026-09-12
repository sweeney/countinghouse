package prices

import (
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Calendar edge cases.
//
// All of this code asks the CALENDAR for the next midnight rather than adding
// 24 hours, which makes leap years, month lengths and year boundaries correct by
// construction. These tests exist to pin that, because "add 24h" looks like an
// obvious simplification to anybody reading it later — and it is wrong on four
// days a year, two of which only occur in some years.
//
// Verified against the tz database rather than assumed:
//
//	clock changes   2026: Mar 29 (23h), Oct 25 (25h)
//	                2027: Mar 28 (23h), Oct 31 (25h)
//	                2028: Mar 26 (23h), Oct 29 (25h)
//	leap years      2028 and 2024 are; 2026 and 2027 are not
//
// Note what this shows about leap days specifically: UK clock changes are always
// in March and October, so 29 February is never one. A leap day is therefore an
// ordinary 48-slot day, and leap years do not affect slot counts at all. What
// they DO affect is date ARITHMETIC — what "tomorrow" is on 28 February, and how
// many days a sweep covers — which is what the collector's tests cover.
// ---------------------------------------------------------------------------

func londonTZ(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Skipf("no tzdata: %v", err)
	}
	return loc
}

// ExpectedSlots must come from the local calendar, across every shape of day the
// calendar produces — not just the two DST days of one convenient year.
func TestExpectedSlotsAcrossTheCalendar(t *testing.T) {
	loc := londonTZ(t)

	for _, tc := range []struct {
		name  string
		day   time.Time
		slots int
	}{
		// Clock changes, three consecutive years. The dates move, so hardcoding
		// one year's would pass today and rot.
		{name: "2026 spring forward", day: time.Date(2026, 3, 29, 12, 0, 0, 0, loc), slots: 46},
		{name: "2026 autumn back", day: time.Date(2026, 10, 25, 12, 0, 0, 0, loc), slots: 50},
		{name: "2027 spring forward", day: time.Date(2027, 3, 28, 12, 0, 0, 0, loc), slots: 46},
		{name: "2027 autumn back", day: time.Date(2027, 10, 31, 12, 0, 0, 0, loc), slots: 50},
		{name: "2028 spring forward, in a leap year", day: time.Date(2028, 3, 26, 12, 0, 0, 0, loc), slots: 46},
		{name: "2028 autumn back, in a leap year", day: time.Date(2028, 10, 29, 12, 0, 0, 0, loc), slots: 50},

		// The day either side of a change is ordinary — an off-by-one in the
		// changeover logic would show up here rather than on the day itself.
		{name: "the day before spring forward", day: time.Date(2026, 3, 28, 12, 0, 0, 0, loc), slots: 48},
		{name: "the day after spring forward", day: time.Date(2026, 3, 30, 12, 0, 0, 0, loc), slots: 48},
		{name: "the day before autumn back", day: time.Date(2026, 10, 24, 12, 0, 0, 0, loc), slots: 48},
		{name: "the day after autumn back", day: time.Date(2026, 10, 26, 12, 0, 0, 0, loc), slots: 48},

		// A leap day is an ORDINARY day. Clock changes are in March and October,
		// so 29 February is never one.
		{name: "leap day 2028", day: time.Date(2028, 2, 29, 12, 0, 0, 0, loc), slots: 48},
		{name: "leap day 2024", day: time.Date(2024, 2, 29, 12, 0, 0, 0, loc), slots: 48},
		{name: "28 February in a leap year", day: time.Date(2028, 2, 28, 12, 0, 0, 0, loc), slots: 48},
		{name: "28 February in a non-leap year", day: time.Date(2027, 2, 28, 12, 0, 0, 0, loc), slots: 48},

		// Month and year boundaries.
		{name: "31 December", day: time.Date(2026, 12, 31, 12, 0, 0, 0, loc), slots: 48},
		{name: "1 January", day: time.Date(2027, 1, 1, 12, 0, 0, 0, loc), slots: 48},
		{name: "last day of a 30-day month", day: time.Date(2026, 4, 30, 12, 0, 0, 0, loc), slots: 48},
		{name: "last day of a 31-day month", day: time.Date(2026, 7, 31, 12, 0, 0, 0, loc), slots: 48},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExpectedSlots(tc.day, loc); got != tc.slots {
				t.Errorf("ExpectedSlots = %d, want %d", got, tc.slots)
			}
		})
	}
}

// The window's END must be the next CALENDAR day's midnight, which is what makes
// leap days, month lengths and year rollover correct without special cases.
func TestLocalDayWindowRollsOverCorrectly(t *testing.T) {
	loc := londonTZ(t)

	for _, tc := range []struct {
		name     string
		day      time.Time
		wantNext string // the local date the window should end on
	}{
		{
			name: "28 February in a LEAP year rolls to the 29th",
			day:  time.Date(2028, 2, 28, 12, 0, 0, 0, loc), wantNext: "2028-02-29",
		},
		{
			name: "28 February in a NON-leap year rolls to 1 March",
			day:  time.Date(2027, 2, 28, 12, 0, 0, 0, loc), wantNext: "2027-03-01",
		},
		{
			name: "29 February rolls to 1 March",
			day:  time.Date(2028, 2, 29, 12, 0, 0, 0, loc), wantNext: "2028-03-01",
		},
		{
			name: "31 December rolls into the next year",
			day:  time.Date(2026, 12, 31, 12, 0, 0, 0, loc), wantNext: "2027-01-01",
		},
		{
			name: "30 April rolls to 1 May",
			day:  time.Date(2026, 4, 30, 12, 0, 0, 0, loc), wantNext: "2026-05-01",
		},
		{
			name: "31 January rolls to 1 February",
			day:  time.Date(2026, 1, 31, 12, 0, 0, 0, loc), wantNext: "2026-02-01",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			start, end := LocalDayWindow(tc.day, loc)
			if got := end.In(loc).Format("2006-01-02"); got != tc.wantNext {
				t.Errorf("window ends on %s, want %s", got, tc.wantNext)
			}
			if got := end.In(loc).Format("15:04"); got != "00:00" {
				t.Errorf("window ends at %s local, want midnight", got)
			}
			if !end.After(start) {
				t.Errorf("window [%s, %s) is not ordered", start, end)
			}
			// Both bounds are UTC instants, whatever zone the question was asked in.
			if start.Location() != time.UTC || end.Location() != time.UTC {
				t.Errorf("bounds are %v/%v, want UTC", start.Location(), end.Location())
			}
		})
	}
}

// A full leap day must read as complete. If leap handling were wrong anywhere,
// 29 February would be permanently short or permanently over.
func TestCheckDayOnALeapDay(t *testing.T) {
	loc := londonTZ(t)
	day := time.Date(2028, 2, 29, 12, 0, 0, 0, loc)
	start, end := LocalDayWindow(day, loc)

	var slots []Slot
	for cur := start; cur.Before(end); cur = cur.Add(30 * time.Minute) {
		to := cur.Add(30 * time.Minute)
		slots = append(slots, Slot{
			TariffCode: tariffA, ValidFrom: cur, ValidTo: &to,
			ExcVATPence: 20, IncVATPence: 21,
			RetrievedAt: time.Date(2028, 2, 28, 16, 5, 0, 0, time.UTC),
		})
	}

	got := CheckDay(slots, day, loc)
	if !got.Complete {
		t.Errorf("a full leap day should be complete: %+v", got)
	}
	if got.Expected != 48 || got.Present != 48 {
		t.Errorf("present %d of %d, want 48 of 48", got.Present, got.Expected)
	}
	if len(got.Missing) != 0 || len(got.Overlaps) != 0 {
		t.Errorf("missing %v, overlaps %v, want neither", got.Missing, got.Overlaps)
	}
}

// A day is complete whatever its length, which is the whole reason the count is
// computed. Run the full set rather than trusting the 48-slot case.
func TestCheckDayCompleteAtEveryDayLength(t *testing.T) {
	loc := londonTZ(t)

	for _, tc := range []struct {
		name  string
		day   time.Time
		slots int
	}{
		{name: "23-hour day", day: time.Date(2026, 3, 29, 12, 0, 0, 0, loc), slots: 46},
		{name: "24-hour day", day: time.Date(2026, 6, 15, 12, 0, 0, 0, loc), slots: 48},
		{name: "25-hour day", day: time.Date(2026, 10, 25, 12, 0, 0, 0, loc), slots: 50},
	} {
		t.Run(tc.name, func(t *testing.T) {
			start, end := LocalDayWindow(tc.day, loc)
			var slots []Slot
			for cur := start; cur.Before(end); cur = cur.Add(30 * time.Minute) {
				to := cur.Add(30 * time.Minute)
				slots = append(slots, Slot{
					TariffCode: tariffA, ValidFrom: cur, ValidTo: &to,
					ExcVATPence: 20, IncVATPence: 21, RetrievedAt: start,
				})
			}
			if len(slots) != tc.slots {
				t.Fatalf("built %d slots, want %d — the fixture itself disagrees with the calendar",
					len(slots), tc.slots)
			}
			got := CheckDay(slots, tc.day, loc)
			if !got.Complete {
				t.Errorf("a full %d-slot day should be complete: %+v", tc.slots, got)
			}
		})
	}
}
