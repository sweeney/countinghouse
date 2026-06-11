package testutil

import (
	"testing"
	"time"
)

func TestFakeClockNow(t *testing.T) {
	start := time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)
	c := NewFakeClock(start)
	if got := c.Now(); !got.Equal(start) {
		t.Fatalf("Now() = %v, want %v", got, start)
	}
}

func TestNewFakeClockNormalisesToUTC(t *testing.T) {
	loc, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Fatalf("load location: %v", err)
	}
	// 13:00 BST == 12:00 UTC.
	start := time.Date(2026, 6, 11, 13, 0, 0, 0, loc)
	c := NewFakeClock(start)
	got := c.Now()
	if got.Location() != time.UTC {
		t.Fatalf("Now().Location() = %v, want UTC", got.Location())
	}
	want := time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("Now() = %v, want %v", got, want)
	}
}

func TestFakeClockSet(t *testing.T) {
	c := NewFakeClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	want := time.Date(2026, 12, 25, 9, 30, 0, 0, time.UTC)
	c.Set(want)
	if got := c.Now(); !got.Equal(want) {
		t.Fatalf("after Set, Now() = %v, want %v", got, want)
	}
}

func TestFakeClockAdvance(t *testing.T) {
	start := time.Date(2026, 6, 11, 0, 0, 0, 0, time.UTC)
	c := NewFakeClock(start)
	c.Advance(90 * time.Minute)
	want := start.Add(90 * time.Minute)
	if got := c.Now(); !got.Equal(want) {
		t.Fatalf("after Advance, Now() = %v, want %v", got, want)
	}
}

func TestRealClockReturnsUTC(t *testing.T) {
	if got := (RealClock{}).Now().Location(); got != time.UTC {
		t.Fatalf("RealClock.Now().Location() = %v, want UTC", got)
	}
}
