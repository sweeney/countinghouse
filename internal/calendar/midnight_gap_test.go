package calendar

import (
	"math"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Zones whose clocks change AT local midnight (PR #57 review).
//
// Europe/London changes at 01:00/02:00, so its midnight always exists and the
// London tables never exercise this. America/Santiago and America/Havana spring
// forward at 00:00: that day has no 00:00 at all, and time.Date resolves the
// missing midnight BACKWARDS to 23:00 the previous day. Days used to step to
// "the next midnight" from that instant, land on the same instant, and spin
// forever — on the request path, with no ctx to stop it.
//
// config.checkWholeHourZone accepts both zones by name, so these are reachable
// configurations, not curiosities.
// ---------------------------------------------------------------------------

func mustLoad(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("load %s: %v", name, err)
	}
	return loc
}

// daysWithin runs Days under a watchdog, so a regression to the infinite loop
// FAILS this test rather than hanging the whole package until go test's
// ten-minute timeout.
func daysWithin(t *testing.T, from, to time.Time, loc *time.Location) float64 {
	t.Helper()
	done := make(chan float64, 1)
	go func() { done <- Days(from, to, loc) }()
	select {
	case got := <-done:
		return got
	case <-time.After(2 * time.Second):
		t.Fatalf("Days(%s, %s, %s) did not return: the day walk is not advancing",
			from.Format(time.RFC3339), to.Format(time.RFC3339), loc)
		return 0
	}
}

func TestDaysWhereLocalMidnightIsSkippedOrRepeated(t *testing.T) {
	santiago := mustLoad(t, "America/Santiago")
	havana := mustLoad(t, "America/Havana")

	// Spans are given by their real instants, not via time.Date(…, 0, 0, …) on
	// the gap day — that is the very call whose resolution is in question.
	tests := []struct {
		name     string
		loc      *time.Location
		from, to time.Time
		want     float64
	}{
		{
			// Sun 6 Sep 2026: 00:00 -04 becomes 01:00 -03, so Sunday is 23 hours
			// and starts at 01:00. Sat 00:00 -04 to Mon 00:00 -03 is two days.
			name: "Santiago, Sat 5 to Mon 7 Sep 2026 across the skipped midnight",
			loc:  santiago,
			from: time.Date(2026, 9, 5, 4, 0, 0, 0, time.UTC),
			to:   time.Date(2026, 9, 7, 3, 0, 0, 0, time.UTC),
			want: 2,
		},
		{
			// The gap day alone: it starts at the transition (04:00Z = 01:00 -03).
			name: "Santiago, Sun 6 Sep 2026 alone (23h, starts at 01:00)",
			loc:  santiago,
			from: time.Date(2026, 9, 6, 4, 0, 0, 0, time.UTC),
			to:   time.Date(2026, 9, 7, 3, 0, 0, 0, time.UTC),
			want: 1,
		},
		{
			// Half the gap day: 11.5 of its 23 hours.
			name: "Santiago, first half of the 23h gap day",
			loc:  santiago,
			from: time.Date(2026, 9, 6, 4, 0, 0, 0, time.UTC),
			to:   time.Date(2026, 9, 6, 15, 30, 0, 0, time.UTC),
			want: 0.5,
		},
		{
			// Sun 8 Mar 2026: 00:00 -05 becomes 01:00 -04.
			name: "Havana, Sun 8 Mar 2026 whole day (midnight skipped)",
			loc:  havana,
			from: time.Date(2026, 3, 8, 5, 0, 0, 0, time.UTC),
			to:   time.Date(2026, 3, 9, 4, 0, 0, 0, time.UTC),
			want: 1,
		},
		{
			// Clocks go BACK at midnight into Sun 5 Apr 2026: Saturday's 24:00 -03
			// becomes 23:00 -04, so Saturday's last hour repeats and Saturday is
			// 25 hours — still one day.
			name: "Santiago, Sat 4 Apr 2026 whole day (25h, clocks back at midnight)",
			loc:  santiago,
			from: time.Date(2026, 4, 4, 3, 0, 0, 0, time.UTC),
			to:   time.Date(2026, 4, 5, 4, 0, 0, 0, time.UTC),
			want: 1,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := daysWithin(t, tc.from, tc.to, tc.loc)
			if math.Abs(got-tc.want) > 1e-12 {
				t.Errorf("Days = %.12f, want %.12f", got, tc.want)
			}
		})
	}
}

// The reviewer's reproduction: ±48h around the gap day, three years running,
// in both zones. Every one of these used to hang.
func TestDaysTerminatesAroundEveryMidnightGap(t *testing.T) {
	gaps := []struct {
		zone string
		at   time.Time // an instant on the gap day, after the transition
	}{
		{"America/Santiago", time.Date(2025, 9, 7, 12, 0, 0, 0, time.UTC)},
		{"America/Santiago", time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)},
		{"America/Santiago", time.Date(2027, 9, 5, 12, 0, 0, 0, time.UTC)},
		{"America/Havana", time.Date(2025, 3, 9, 12, 0, 0, 0, time.UTC)},
		{"America/Havana", time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)},
		{"America/Havana", time.Date(2027, 3, 14, 12, 0, 0, 0, time.UTC)},
	}
	for _, g := range gaps {
		loc := mustLoad(t, g.zone)
		t.Run(g.zone+" "+g.at.Format("2006-01-02"), func(t *testing.T) {
			got := daysWithin(t, g.at.Add(-48*time.Hour), g.at.Add(48*time.Hour), loc)
			// Four days of span, give or take the gap day's missing hour being
			// worth 1/23 rather than 1/24: just check it is sane and finite.
			if got < 3.9 || got > 4.1 {
				t.Errorf("Days over 96h around the gap = %v, want ~4", got)
			}
		})
	}
}

// Additivity across the gap: the half-hourly sweep from the London table,
// repeated in Santiago across early September 2026.
func TestDaysIsAdditiveAcrossAMidnightGap(t *testing.T) {
	loc := mustLoad(t, "America/Santiago")
	from := time.Date(2026, 9, 5, 4, 0, 0, 0, time.UTC) // Sat 00:00 -04
	to := time.Date(2026, 9, 8, 3, 0, 0, 0, time.UTC)   // Tue 00:00 -03
	whole := daysWithin(t, from, to, loc)
	if math.Abs(whole-3) > 1e-12 {
		t.Fatalf("Sat..Tue = %.12f, want 3", whole)
	}
	for cut := from.Add(30 * time.Minute); cut.Before(to); cut = cut.Add(30 * time.Minute) {
		split := daysWithin(t, from, cut, loc) + daysWithin(t, cut, to, loc)
		if math.Abs(split-whole) > 1e-12 {
			t.Fatalf("cut at %s: split %.12f, whole %.12f", cut.Format(time.RFC3339), split, whole)
		}
	}
}

// startOfDay directly: the instant each civil date begins, on ordinary
// midnights and on both kinds of midnight transition.
func TestStartOfDay(t *testing.T) {
	london := mustLoad(t, "Europe/London")
	santiago := mustLoad(t, "America/Santiago")
	havana := mustLoad(t, "America/Havana")

	tests := []struct {
		name    string
		y       int
		m       time.Month
		d       int
		loc     *time.Location
		wantUTC time.Time
	}{
		{"London ordinary GMT midnight", 2026, 10, 26, london, time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)},
		{"London ordinary BST midnight", 2026, 10, 25, london, time.Date(2026, 10, 24, 23, 0, 0, 0, time.UTC)},
		// Skipped midnight: the date begins at the transition, 01:00 -03 / -04.
		{"Santiago skipped midnight", 2026, 9, 6, santiago, time.Date(2026, 9, 6, 4, 0, 0, 0, time.UTC)},
		{"Havana skipped midnight", 2026, 3, 8, havana, time.Date(2026, 3, 8, 5, 0, 0, 0, time.UTC)},
		// Clocks go BACK at 00:00 -03 to 23:00 -04: what repeats is Saturday's
		// last hour, so Sunday's 00:00 happens once, at -04, and Saturday is the
		// 25-hour day.
		{"Santiago clocks go back at midnight", 2026, 4, 5, santiago, time.Date(2026, 4, 5, 4, 0, 0, 0, time.UTC)},
		// Overflowing fields normalise as time.Date does: 32 Oct is 1 Nov.
		{"day overflow normalises", 2026, 10, 32, london, time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := startOfDay(tc.y, tc.m, tc.d, tc.loc)
			if !got.Equal(tc.wantUTC) {
				t.Errorf("startOfDay = %s (%s), want %s",
					got.Format(time.RFC3339), got.UTC().Format(time.RFC3339), tc.wantUTC.Format(time.RFC3339))
			}
		})
	}
}
