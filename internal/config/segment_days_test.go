package config

import (
	"math"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Segment.Days on clock-change days (issue #56).
//
// This is the CONFIGURED-tariff path: StandingChargeAcross multiplies each
// segment's Days by its daily_standing_charge. It is used whenever the archive
// cannot answer — no archive, a read failure, partial coverage, a fixed tariff —
// so it has to agree with the archive path (prices.Schedule.ChargeOver) on what
// a day is, or the same window bills differently depending on which source
// happened to be available. Both now delegate to calendar.Days; these cases are
// the same ones the archive path is held to.
// ---------------------------------------------------------------------------

func TestSegmentDaysCountsCalendarDays(t *testing.T) {
	loc, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Fatal(err)
	}
	at := func(y int, m time.Month, d, h int) time.Time { return time.Date(y, m, d, h, 0, 0, 0, loc) }

	tests := []struct {
		name        string
		start, stop time.Time
		want        float64
	}{
		{"25 Oct 2026 whole (25h) is one day", at(2026, 10, 25, 0), at(2026, 10, 26, 0), 1.0},
		{"29 Mar 2026 whole (23h) is one day", at(2026, 3, 29, 0), at(2026, 3, 30, 0), 1.0},
		{"25 Oct first 12 elapsed hours is 12/25", at(2026, 10, 25, 0), at(2026, 10, 25, 0).Add(12 * time.Hour), 12.0 / 25.0},
		{"25 Oct 00:00 local to 23:00Z is 24/25", at(2026, 10, 25, 0), time.Date(2026, 10, 25, 23, 0, 0, 0, time.UTC), 24.0 / 25.0},
		{"24-26 Oct whole days is 2", at(2026, 10, 24, 0), at(2026, 10, 26, 0), 2.0},
		{"26 Oct 00:00-12:00 (ordinary day) is 0.5", at(2026, 10, 26, 0), at(2026, 10, 26, 12), 0.5},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// UTC instants, as PeriodsBetween may hand them over: the calendar
			// must come from loc, not from the Segment's own time.Location.
			seg := Segment{Start: tc.start.UTC(), Stop: tc.stop.UTC()}
			if got := seg.Days(loc); math.Abs(got-tc.want) > 1e-12 {
				t.Errorf("Days = %.12f, want %.12f", got, tc.want)
			}
		})
	}
}
