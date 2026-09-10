package influx

import (
	"math"
	"testing"
	"time"
)

// CounterSim is the model the endpoint-agreement regressions for issue #27 are
// argued from: if it does not reproduce Influx's arithmetic, those tests prove
// nothing. So it is exercised through the REAL builders rather than hand-written
// Flux, which also means a change to a builder's query string cannot silently
// break the sim's parsing.

func simLondon(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Fatalf("LoadLocation(Europe/London): %v", err)
	}
	return loc
}

// steadySim is 1 kW on "winefridge", sampled every minute over 12:00-18:00.
func steadySim(t *testing.T) (*CounterSim, *time.Location) {
	t.Helper()
	loc := simLondon(t)
	return NewCounterSim(loc).AddSteady("winefridge",
		time.Date(2026, 6, 11, 12, 0, 0, 0, loc),
		time.Date(2026, 6, 11, 18, 0, 0, 0, loc),
		time.Minute, 1000), loc
}

// The whole-window reduction is the counter's rise between the first and last
// reading INSIDE the range — increase() re-bases at the first point it sees.
func TestCounterSimWholeWindowReduction(t *testing.T) {
	sim, loc := steadySim(t)
	start := time.Date(2026, 6, 11, 14, 29, 0, 0, loc)
	stop := time.Date(2026, 6, 11, 16, 43, 0, 0, loc)

	rows, err := sim.Answer(BuildCounterFlux("b", "winefridge", start, stop))
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1 (last() collapses to one row per device)", len(rows))
	}
	// 14:29 to the last minute-sample strictly inside the window (16:42).
	if want := 133.0 / 60; math.Abs(rows[0].Value-want) > 1e-9 {
		t.Errorf("value = %v, want %v", rows[0].Value, want)
	}
	if rows[0].DeviceID != "winefridge" {
		t.Errorf("device_id = %q", rows[0].DeviceID)
	}
}

// A range holding a single reading has nothing to rise from: increase() re-bases
// there and yields 0. This is not an artefact of the sim — it is why the clipped
// first bucket can legitimately read 0 for a sub-cadence head, and why the
// series total still telescopes to the scalar answer.
func TestCounterSimSingleSampleRangeIsZero(t *testing.T) {
	sim, loc := steadySim(t)
	rows, err := sim.Answer(BuildCounterHeadFlux("b", []string{"winefridge"},
		time.Date(2026, 6, 11, 14, 29, 0, 0, loc),
		time.Date(2026, 6, 11, 14, 30, 0, 0, loc)))
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}
	if len(rows) != 1 || rows[0].Value != 0 {
		t.Fatalf("rows = %+v, want a single 0-valued row", rows)
	}
}

// The bucketed shape: one row per grid window AFTER the first (difference()
// consumes the first as its seed), stamped at the window's LEFT edge, carrying
// that window's delta. The grid is anchored at local midnight, NOT at the range
// start — which is the whole mechanism behind issue #27.
func TestCounterSimWindowedShape(t *testing.T) {
	sim, loc := steadySim(t)
	// padStart takes this back to 13:59, so the seed window is 13:30.
	start := time.Date(2026, 6, 11, 14, 29, 0, 0, loc)
	stop := time.Date(2026, 6, 11, 16, 0, 0, 0, loc)

	rows, err := sim.Answer(BuildCounterSeriesFlux("b", []string{"winefridge"}, start, stop, "30m", "Europe/London"))
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}
	want := []time.Time{
		time.Date(2026, 6, 11, 14, 0, 0, 0, loc),
		time.Date(2026, 6, 11, 14, 30, 0, 0, loc),
		time.Date(2026, 6, 11, 15, 0, 0, 0, loc),
		time.Date(2026, 6, 11, 15, 30, 0, 0, loc),
	}
	if len(rows) != len(want) {
		t.Fatalf("rows = %d (%+v), want %d", len(rows), rows, len(want))
	}
	for i, w := range want {
		if !rows[i].Time.Equal(w) {
			t.Errorf("row %d stamped %v, want left edge %v", i, rows[i].Time, w)
		}
	}
	// Every delta, bucket 0 included, is a full 30m interval at 1 kW. That is the
	// defect issue #27 reports, faithfully reproduced: the seed window closes at
	// 13:59 and bucket 0 closes at 14:29, so bucket 0 reports the whole
	// [14:00, 14:30) grid interval and is indistinguishable from a full one —
	// which is why the reported total did not move as `from` did.
	for i, r := range rows {
		if math.Abs(r.Value-0.5) > 1e-9 {
			t.Errorf("delta %d = %v, want 0.5 (a full 30m grid interval at 1 kW)", i, r.Value)
		}
	}
}

// Only devices named in the query answer, and only the energy_kwh field.
func TestCounterSimFiltersDevicesAndField(t *testing.T) {
	loc := simLondon(t)
	from := time.Date(2026, 6, 11, 12, 0, 0, 0, loc)
	to := time.Date(2026, 6, 11, 18, 0, 0, 0, loc)
	sim := NewCounterSim(loc).
		AddSteady("winefridge", from, to, time.Minute, 1000).
		AddSteady("freezer", from, to, time.Minute, 200)

	rows, err := sim.Answer(BuildCounterHeadFlux("b", []string{"freezer"}, from, to))
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}
	if len(rows) != 1 || rows[0].DeviceID != "freezer" {
		t.Fatalf("rows = %+v, want only freezer", rows)
	}

	// A power_w query is not this sim's field: no rows, no error, so a test can
	// compose it with its own power telemetry.
	rows, err = sim.Answer(BuildPowerMeanSeriesFlux("b", []string{"freezer"}, from, to, "30m", "Europe/London"))
	if err != nil || rows != nil {
		t.Fatalf("power query = (%+v, %v), want (nil, nil)", rows, err)
	}
}

// A counter with no readings in the range answers with no rows, as an offline
// device does — not with a zero that would read as "measured, and it was zero".
func TestCounterSimEmptyRangeYieldsNoRows(t *testing.T) {
	sim, loc := steadySim(t)
	rows, err := sim.Answer(BuildCounterHeadFlux("b", []string{"winefridge"},
		time.Date(2026, 6, 12, 3, 0, 0, 0, loc),
		time.Date(2026, 6, 12, 4, 0, 0, 0, loc)))
	if err != nil || rows != nil {
		t.Fatalf("out-of-range query = (%+v, %v), want (nil, nil)", rows, err)
	}
}

// An interval outside the allowed set is an error, not a silent fallback: a sim
// that quietly bucketed at the wrong width would make a green test meaningless.
func TestCounterSimRejectsUnknownInterval(t *testing.T) {
	sim, loc := steadySim(t)
	flux := BuildCounterSeriesFlux("b", []string{"winefridge"},
		time.Date(2026, 6, 11, 14, 0, 0, 0, loc),
		time.Date(2026, 6, 11, 16, 0, 0, 0, loc), "7m", "Europe/London")
	if _, err := sim.Answer(flux); err == nil {
		t.Error("unknown interval should be an error, not a fallback")
	}
}

// The daily grid steps by CALENDAR day, so the London day that gains an hour at
// the autumn changeover is still one window starting at local midnight — the
// DST-aware behaviour aggregateWindow(location:) has.
func TestCounterSimDailyGridStepsCalendarDaysAcrossDST(t *testing.T) {
	loc := simLondon(t)
	// 2026-10-25 is the London autumn changeover; that local day is 25h.
	from := time.Date(2026, 10, 23, 0, 0, 0, 0, loc)
	to := time.Date(2026, 10, 28, 0, 0, 0, 0, loc)
	sim := NewCounterSim(loc).AddSteady("winefridge", from, to, time.Hour, 1000)

	rows, err := sim.Answer(BuildCounterSeriesFlux("b", []string{"winefridge"},
		time.Date(2026, 10, 24, 0, 0, 0, 0, loc),
		time.Date(2026, 10, 27, 0, 0, 0, 0, loc), "1d", "Europe/London"))
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}
	for i, r := range rows {
		if h, m, s := r.Time.In(loc).Clock(); h != 0 || m != 0 || s != 0 {
			t.Errorf("row %d stamped %v, want a local midnight", i, r.Time.In(loc))
		}
	}
	// The 25th's window is 25 hours long, so it carries an extra kWh at 1 kW.
	var dstDay float64
	for _, r := range rows {
		if d := r.Time.In(loc).Day(); d == 25 {
			dstDay = r.Value
		}
	}
	if math.Abs(dstDay-25.0) > 1e-9 {
		t.Errorf("the DST day's delta = %v kWh, want 25 (a 25h London day at 1 kW)", dstDay)
	}
}
