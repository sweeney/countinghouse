package energy

import (
	"strings"
	"testing"
	"time"
)

func TestDefaultInterval(t *testing.T) {
	loc := mustLondon(t)
	now := time.Date(2026, 6, 11, 14, 0, 0, 0, loc)

	cases := []struct {
		spec string
		from time.Time
		to   time.Time
		want string
	}{
		{spec: WindowToday, want: "1h"},
		{spec: WindowWeek, want: "1d"},
		{spec: WindowMonth, want: "1d"},
		// custom spans:
		{spec: WindowCustom, from: now, to: now.Add(24 * time.Hour), want: "1h"},      // ≤2d
		{spec: WindowCustom, from: now, to: now.Add(10 * 24 * time.Hour), want: "6h"}, // ≤14d
		{spec: WindowCustom, from: now, to: now.Add(60 * 24 * time.Hour), want: "1d"}, // big
	}
	for _, c := range cases {
		win, err := ResolveWindow(now, loc, c.spec, c.from, c.to)
		if err != nil {
			t.Fatalf("ResolveWindow(%s): %v", c.spec, err)
		}
		if got := DefaultInterval(win); got != c.want {
			t.Errorf("DefaultInterval(%s span) = %q, want %q", c.spec, got, c.want)
		}
	}
}

func TestResolveIntervalDefaultsWhenEmpty(t *testing.T) {
	loc := mustLondon(t)
	now := time.Date(2026, 6, 11, 14, 0, 0, 0, loc)
	win, _ := ResolveWindow(now, loc, WindowToday, time.Time{}, time.Time{})

	iv, err := ResolveInterval(win, "", loc)
	if err != nil {
		t.Fatalf("ResolveInterval empty: %v", err)
	}
	if iv.Token != "1h" {
		t.Errorf("default today interval = %q, want 1h", iv.Token)
	}
}

func TestResolveIntervalAllowedAndDisallowed(t *testing.T) {
	loc := mustLondon(t)
	now := time.Date(2026, 6, 11, 14, 0, 0, 0, loc)
	win, _ := ResolveWindow(now, loc, WindowToday, time.Time{}, time.Time{})

	for _, tok := range []string{"5m", "15m", "30m", "1h", "6h", "1d"} {
		iv, err := ResolveInterval(win, tok, loc)
		if err != nil {
			t.Errorf("interval %q should be allowed: %v", tok, err)
		}
		if iv.Token != tok {
			t.Errorf("token = %q, want %q", iv.Token, tok)
		}
	}

	for _, bad := range []string{"2m", "1m", "12h", "1w", "nonsense"} {
		if _, err := ResolveInterval(win, bad, loc); err == nil {
			t.Errorf("interval %q should be rejected", bad)
		}
	}
}

func TestResolveIntervalCalendarFlag(t *testing.T) {
	loc := mustLondon(t)
	now := time.Date(2026, 6, 11, 14, 0, 0, 0, loc)
	win, _ := ResolveWindow(now, loc, WindowWeek, time.Time{}, time.Time{})

	day, err := ResolveInterval(win, "1d", loc)
	if err != nil {
		t.Fatalf("ResolveInterval 1d: %v", err)
	}
	if !day.Calendar {
		t.Error("1d interval should be Calendar")
	}

	hour, _ := ResolveInterval(win, "1h", loc)
	if hour.Calendar {
		t.Error("1h interval should NOT be Calendar")
	}
	if hour.Duration != time.Hour {
		t.Errorf("1h Duration = %v", hour.Duration)
	}
}

func TestResolveIntervalMaxBucketsCap(t *testing.T) {
	loc := mustLondon(t)
	now := time.Date(2026, 6, 11, 0, 0, 0, 0, loc)
	// 60-day custom window at 5m → 17280 buckets, well over the 1000 cap.
	from := now
	to := now.Add(60 * 24 * time.Hour)
	win, err := ResolveWindow(now, loc, WindowCustom, from, to)
	if err != nil {
		t.Fatalf("ResolveWindow custom: %v", err)
	}

	_, err = ResolveInterval(win, "5m", loc)
	if err == nil {
		t.Fatal("5m over 60 days should exceed MaxBuckets")
	}
	if !strings.Contains(err.Error(), "exceeding the cap") {
		t.Errorf("error should mention the cap: %v", err)
	}
	// Suggestion should point at a coarser allowed interval.
	if !strings.Contains(err.Error(), "coarser interval") {
		t.Errorf("error should suggest a coarser interval: %v", err)
	}

	// 1d over the same window is well within the cap.
	if _, err := ResolveInterval(win, "1d", loc); err != nil {
		t.Errorf("1d over 60 days should be within cap: %v", err)
	}
}

func TestResolveIntervalJustUnderCap(t *testing.T) {
	loc := mustLondon(t)
	now := time.Date(2026, 6, 11, 0, 0, 0, 0, loc)
	// 3 days at 5m = 864 buckets (< 1000): allowed.
	win, _ := ResolveWindow(now, loc, WindowCustom, now, now.Add(3*24*time.Hour))
	if _, err := ResolveInterval(win, "5m", loc); err != nil {
		t.Errorf("864 buckets should be under cap: %v", err)
	}
}

func TestAllowedIntervals(t *testing.T) {
	got := AllowedIntervals()
	want := []string{"5m", "15m", "30m", "1h", "6h", "1d"}
	if len(got) != len(want) {
		t.Fatalf("AllowedIntervals = %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("AllowedIntervals[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestResolveIntervalNilLocation(t *testing.T) {
	if _, err := ResolveInterval(Window{}, "1h", nil); err == nil {
		t.Fatal("nil location should error")
	}
}
