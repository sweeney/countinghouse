package energy

import (
	"testing"
	"time"

	"github.com/sweeney/countinghouse/internal/testutil"
)

// mustLondon loads Europe/London or fails the test.
func mustLondon(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Fatalf("LoadLocation(Europe/London): %v", err)
	}
	return loc
}

// utc builds a UTC instant.
func utc(y int, mo time.Month, d, h, mi int) time.Time {
	return time.Date(y, mo, d, h, mi, 0, 0, time.UTC)
}

// resolve drives the resolver via a FakeClock to prove no time.Now() is used.
func resolve(t *testing.T, now time.Time, loc *time.Location, spec string, from, to time.Time) Window {
	t.Helper()
	clk := testutil.NewFakeClock(now)
	w, err := ResolveWindow(clk.Now(), loc, spec, from, to)
	if err != nil {
		t.Fatalf("ResolveWindow(%q): unexpected error: %v", spec, err)
	}
	return w
}

func TestResolveWindow_TodayWeekMonth(t *testing.T) {
	loc := mustLondon(t)
	// 2026-06-11 is a Thursday. In June, London is BST (UTC+1), so 14:30 UTC
	// is 15:30 local.
	now := utc(2026, 6, 11, 14, 30)

	tests := []struct {
		spec  string
		start time.Time // expected, in local terms
		label string
	}{
		// Thursday -> today's local midnight.
		{WindowToday, time.Date(2026, 6, 11, 0, 0, 0, 0, loc), WindowToday},
		// Week starting Monday: 2026-06-08 is the Monday of that week.
		{WindowWeek, time.Date(2026, 6, 8, 0, 0, 0, 0, loc), WindowWeek},
		// Month: 1st of June.
		{WindowMonth, time.Date(2026, 6, 1, 0, 0, 0, 0, loc), WindowMonth},
	}

	for _, tc := range tests {
		t.Run(tc.spec, func(t *testing.T) {
			w := resolve(t, now, loc, tc.spec, time.Time{}, time.Time{})
			if !w.Start.Equal(tc.start) {
				t.Errorf("Start = %s, want %s", w.Start, tc.start)
			}
			if !w.Stop.Equal(now) {
				t.Errorf("Stop = %s, want now %s", w.Stop, now)
			}
			if w.Label != tc.label {
				t.Errorf("Label = %q, want %q", w.Label, tc.label)
			}
		})
	}
}

func TestResolveWindow_WeekStartsMonday(t *testing.T) {
	loc := mustLondon(t)
	wantMonday := time.Date(2026, 6, 8, 0, 0, 0, 0, loc)

	tests := []struct {
		name string
		now  time.Time
	}{
		{"monday", utc(2026, 6, 8, 9, 0)},     // Monday -> that Monday's midnight
		{"wednesday", utc(2026, 6, 10, 9, 0)}, // Wednesday -> back to Mon 8th
		{"sunday", utc(2026, 6, 14, 20, 0)},   // Sun 14th 21:00 BST -> still Mon 8th
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := resolve(t, tc.now, loc, WindowWeek, time.Time{}, time.Time{})
			if !w.Start.Equal(wantMonday) {
				t.Errorf("week Start = %s, want %s", w.Start, wantMonday)
			}
		})
	}
}

func TestResolveWindow_MonthBoundary(t *testing.T) {
	loc := mustLondon(t)
	wantFirst := time.Date(2026, 6, 1, 0, 0, 0, 0, loc)

	tests := []struct {
		name string
		now  time.Time
	}{
		{"on the 1st", utc(2026, 6, 1, 0, 30)},
		{"mid month", utc(2026, 6, 17, 12, 0)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := resolve(t, tc.now, loc, WindowMonth, time.Time{}, time.Time{})
			if !w.Start.Equal(wantFirst) {
				t.Errorf("month Start = %s, want %s", w.Start, wantFirst)
			}
		})
	}
}

func TestResolveWindow_BSTvsGMTOffset(t *testing.T) {
	loc := mustLondon(t)

	// Summer: BST (UTC+1). Local midnight is the previous day 23:00 UTC.
	summer := resolve(t, utc(2026, 7, 15, 10, 0), loc, WindowToday, time.Time{}, time.Time{})
	_, summerOffset := summer.Start.Zone()
	if summerOffset != 3600 {
		t.Errorf("summer Start offset = %ds, want 3600 (BST UTC+1)", summerOffset)
	}
	if got := summer.Start.UTC(); !got.Equal(utc(2026, 7, 14, 23, 0)) {
		t.Errorf("summer Start in UTC = %s, want 2026-07-14 23:00Z", got)
	}

	// Winter: GMT (UTC+0). Local midnight is the same day 00:00 UTC.
	winter := resolve(t, utc(2026, 1, 15, 10, 0), loc, WindowToday, time.Time{}, time.Time{})
	_, winterOffset := winter.Start.Zone()
	if winterOffset != 0 {
		t.Errorf("winter Start offset = %ds, want 0 (GMT UTC+0)", winterOffset)
	}
	if got := winter.Start.UTC(); !got.Equal(utc(2026, 1, 15, 0, 0)) {
		t.Errorf("winter Start in UTC = %s, want 2026-01-15 00:00Z", got)
	}

	if summerOffset == winterOffset {
		t.Errorf("expected BST and GMT offsets to differ, both = %d", summerOffset)
	}
}

func TestResolveWindow_DSTTransitionDays(t *testing.T) {
	loc := mustLondon(t)

	// dayDuration computes the length of the full local day containing dayStart
	// by resolving a "today" window whose Stop is the next local midnight.
	dayDuration := func(now time.Time) time.Duration {
		w, err := ResolveWindow(now, loc, WindowToday, time.Time{}, time.Time{})
		if err != nil {
			t.Fatalf("ResolveWindow: %v", err)
		}
		ln := w.Start.In(loc)
		nextMidnight := time.Date(ln.Year(), ln.Month(), ln.Day()+1, 0, 0, 0, 0, loc)
		return nextMidnight.Sub(w.Start)
	}

	t.Run("spring forward 23h day", func(t *testing.T) {
		// 2026-03-29: last Sunday in March, clocks go forward -> 23h day.
		now := utc(2026, 3, 29, 22, 0)
		if d := dayDuration(now); d != 23*time.Hour {
			t.Errorf("spring-forward day length = %v, want 23h", d)
		}
	})

	t.Run("autumn back 25h day", func(t *testing.T) {
		// 2026-10-25: last Sunday in October, clocks go back -> 25h day.
		now := utc(2026, 10, 25, 22, 0)
		if d := dayDuration(now); d != 25*time.Hour {
			t.Errorf("autumn-back day length = %v, want 25h", d)
		}
	})
}

func TestWindow_Days(t *testing.T) {
	loc := mustLondon(t)

	t.Run("custom full days", func(t *testing.T) {
		from := utc(2026, 6, 1, 0, 0)
		to := utc(2026, 6, 4, 0, 0)
		w, err := ResolveWindow(utc(2026, 6, 11, 0, 0), loc, WindowCustom, from, to)
		if err != nil {
			t.Fatalf("ResolveWindow: %v", err)
		}
		if got := w.Days(); got != 3 {
			t.Errorf("Days() = %v, want 3", got)
		}
	})

	t.Run("fractional period-to-date", func(t *testing.T) {
		// now = local midnight + 6h -> 0.25 days.
		now := utc(2026, 1, 15, 6, 0) // GMT: local midnight is 00:00 UTC
		w := resolve(t, now, loc, WindowToday, time.Time{}, time.Time{})
		if got := w.Days(); got != 0.25 {
			t.Errorf("Days() = %v, want 0.25", got)
		}
	})
}

func TestResolveWindow_CustomErrors(t *testing.T) {
	loc := mustLondon(t)
	now := utc(2026, 6, 11, 12, 0)
	good := utc(2026, 6, 10, 0, 0)

	tests := []struct {
		name     string
		spec     string
		from, to time.Time
	}{
		{"missing from", WindowCustom, time.Time{}, good},
		{"missing to", WindowCustom, good, time.Time{}},
		{"missing both", WindowCustom, time.Time{}, time.Time{}},
		{"to equals from", WindowCustom, good, good},
		{"to before from", WindowCustom, good, good.Add(-time.Hour)},
		{"unknown spec", "fortnight", time.Time{}, time.Time{}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ResolveWindow(now, loc, tc.spec, tc.from, tc.to); err == nil {
				t.Errorf("expected error, got nil")
			}
		})
	}
}

func TestResolveWindow_NilLocation(t *testing.T) {
	if _, err := ResolveWindow(utc(2026, 6, 11, 12, 0), nil, WindowToday, time.Time{}, time.Time{}); err == nil {
		t.Errorf("expected error for nil location, got nil")
	}
}
