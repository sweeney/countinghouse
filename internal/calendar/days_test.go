package calendar

import (
	"math"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Days: how many BILLING days a span covers (issue #56).
//
// A UK standing charge is a price per calendar day of supply, not per 24 hours
// of it. The supplier bills 25 October 2026 — a 25-hour local day, because the
// clocks go back — as ONE day, and 29 March 2026 — 23 hours — as one day too.
// Countinghouse used to bill them as 25/24 and 23/24 of a day, so October's
// bill came out ~2.5p high and March's ~2.5p low, and only a window covering
// both clock changes hid it.
//
// Every case below is in Europe/London, because that is the zone the house is
// billed in and the only one in which the bug was ever visible.
// ---------------------------------------------------------------------------

func london(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Fatalf("load Europe/London: %v", err)
	}
	return loc
}

func TestDays(t *testing.T) {
	loc := london(t)
	local := func(y int, m time.Month, d, h int) time.Time {
		return time.Date(y, m, d, h, 0, 0, 0, loc)
	}

	tests := []struct {
		name     string
		from, to time.Time
		want     float64
	}{
		// --- The clock-change days themselves: one day each, whatever they last.
		{
			// THE case from the issue. 25 hours of wall-clock time, one day of supply.
			name: "autumn clock change, whole 25h day, is one day",
			from: local(2026, 10, 25, 0), to: local(2026, 10, 26, 0),
			want: 1.0,
		},
		{
			name: "spring clock change, whole 23h day, is one day",
			from: local(2026, 3, 29, 0), to: local(2026, 3, 30, 0),
			want: 1.0,
		},

		// --- Part-days on a long day are pro rata against THAT day's length.
		{
			// 00:00 to 12:00 local on 25 Oct is 13 elapsed hours (the 01:00 hour
			// happens twice), but measured against a 25-hour day. The wall-clock
			// reading 12:00 does not make it half a day: it is 13/25.
			name: "autumn day, local midnight to local noon, is 13/25",
			from: local(2026, 10, 25, 0), to: local(2026, 10, 25, 12),
			want: 13.0 / 25.0,
		},
		{
			// The issue's "first 12 h": twelve ELAPSED hours from local midnight.
			name: "autumn day, first 12 elapsed hours, is 12/25",
			from: local(2026, 10, 25, 0), to: local(2026, 10, 25, 0).Add(12 * time.Hour),
			want: 12.0 / 25.0,
		},
		{
			// 24 elapsed hours from local midnight on a 25-hour day ends at 23:00
			// GMT — with an hour of the day still to go. The old code had already
			// billed a whole day by then.
			name: "autumn day, 24 elapsed hours, is 24/25 not a whole day",
			from: local(2026, 10, 25, 0),
			to:   time.Date(2026, 10, 25, 23, 0, 0, 0, time.UTC),
			want: 24.0 / 25.0,
		},
		{
			name: "spring day, first 12 elapsed hours, is 12/23",
			from: local(2026, 3, 29, 0), to: local(2026, 3, 29, 0).Add(12 * time.Hour),
			want: 12.0 / 23.0,
		},

		// --- Windows spanning a change are whole days, not 49/24 or 47/24.
		{
			name: "24 to 26 Oct, whole local days across the autumn change, is 2",
			from: local(2026, 10, 24, 0), to: local(2026, 10, 26, 0),
			want: 2.0,
		},
		{
			name: "the whole of October 2026 is 31 days",
			from: local(2026, 10, 1, 0), to: local(2026, 11, 1, 0),
			want: 31.0,
		},
		{
			name: "the whole of March 2026 is 31 days",
			from: local(2026, 3, 1, 0), to: local(2026, 4, 1, 0),
			want: 31.0,
		},
		{
			// A window starting and ending mid-day spans three local days: the tail
			// of 24 Oct (12/24), all of 25 Oct (1), the head of 26 Oct (12/24).
			name: "noon 24 Oct to noon 26 Oct is 2 days, not 49/24",
			from: local(2026, 10, 24, 12), to: local(2026, 10, 26, 12),
			want: 2.0,
		},

		{
			// A UTC calendar day across the change, as a client sending UTC query
			// strings would ask for it. 24 elapsed hours, and it lies wholly INSIDE
			// the 25-hour local Sun 25 Oct (which runs 24 Oct 23:00Z to 26 Oct
			// 00:00Z), so it is 24/25 of a day — not 1. Surprising, and correct by
			// the definition: the house is billed in London days, and this span is
			// one of them less its first hour.
			name: "UTC day 25 Oct 00:00Z to 26 Oct 00:00Z is 24/25",
			from: time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC),
			to:   time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC),
			want: 24.0 / 25.0,
		},

		// --- Ordinary days are unchanged: regression guard for every other day
		// of the year, which the fix must not touch.
		{
			name: "ordinary day, 26 Oct 00:00 to 12:00, is half a day",
			from: local(2026, 10, 26, 0), to: local(2026, 10, 26, 12),
			want: 0.5,
		},
		{
			name: "ordinary BST day, 6 hours, is a quarter",
			from: local(2026, 6, 11, 0), to: local(2026, 6, 11, 6),
			want: 0.25,
		},
		{
			// Boundaries not on local midnight — a window given in UTC during BST
			// starts at 01:00 local — still split across local days correctly.
			name: "UTC-midnight boundaries during BST, three days",
			from: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
			to:   time.Date(2026, 6, 4, 0, 0, 0, 0, time.UTC),
			want: 3.0,
		},

		// --- Degenerate spans charge nothing rather than going negative.
		{name: "empty span", from: local(2026, 10, 25, 3), to: local(2026, 10, 25, 3), want: 0},
		{name: "inverted span", from: local(2026, 10, 26, 0), to: local(2026, 10, 25, 0), want: 0},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Days(tc.from, tc.to, loc)
			if math.Abs(got-tc.want) > 1e-12 {
				t.Errorf("Days(%s, %s) = %.12f, want %.12f",
					tc.from.Format(time.RFC3339), tc.to.Format(time.RFC3339), got, tc.want)
			}
		})
	}
}

// A day's charge must not depend on how the span is cut up. The bill splits a
// window at every tariff and VAT boundary, and the archive path again at every
// standing-charge change, so if Days were not additive the same day could cost
// a different amount depending on how many agreements happened to touch it.
func TestDaysIsAdditiveAcrossAnyCut(t *testing.T) {
	loc := london(t)
	from := time.Date(2026, 10, 24, 0, 0, 0, 0, loc)
	to := time.Date(2026, 10, 27, 0, 0, 0, 0, loc)
	whole := Days(from, to, loc)

	// Cut at every half hour across the window, including both sides of the
	// repeated 01:00 hour on 25 Oct.
	for cut := from.Add(30 * time.Minute); cut.Before(to); cut = cut.Add(30 * time.Minute) {
		split := Days(from, cut, loc) + Days(cut, to, loc)
		if math.Abs(split-whole) > 1e-12 {
			t.Fatalf("cut at %s: %.12f + split = %.12f, whole = %.12f",
				cut.Format(time.RFC3339), Days(from, cut, loc), split, whole)
		}
	}
}

// The result depends on the instants and the zone, never on the *time.Location
// the caller's time.Time values happen to carry. A window parsed from a query
// string is in UTC; one built by ResolveWindow is in local time. Both must bill
// the same.
func TestDaysIgnoresTheLocationOfItsArguments(t *testing.T) {
	loc := london(t)
	from := time.Date(2026, 10, 25, 0, 0, 0, 0, loc)
	to := time.Date(2026, 10, 26, 0, 0, 0, 0, loc)
	if a, b := Days(from, to, loc), Days(from.UTC(), to.UTC(), loc); a != b {
		t.Errorf("local args give %v, UTC args give %v", a, b)
	}
}

// A nil location is UTC, where every day is 24 hours, so this collapses to the
// old elapsed/24 arithmetic. It exists so a zero-value caller (a test, a
// Server with no Loc) degrades to the old answer rather than panicking.
func TestDaysNilLocationIsUTC(t *testing.T) {
	from := time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC)
	to := from.Add(36 * time.Hour)
	if got := Days(from, to, nil); got != 1.5 {
		t.Errorf("Days with nil loc = %v, want 1.5", got)
	}
}
