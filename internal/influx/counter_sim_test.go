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
	rows, err := sim.Answer(BuildCounterFlux("b", "winefridge",
		time.Date(2026, 6, 11, 14, 29, 0, 0, loc),
		time.Date(2026, 6, 11, 14, 30, 0, 0, loc)))
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}
	if len(rows) != 1 || rows[0].Value != 0 {
		t.Fatalf("rows = %+v, want a single 0-valued row", rows)
	}
}

// The bucketed shape: one row per grid window that HAS a reading, stamped at the
// window's LEFT edge, carrying that window's closing RUNNING TOTAL measured from
// the first reading in range — not a per-bucket delta, and with no seed window
// consumed. The grid is anchored at local midnight, not at the range start.
func TestCounterSimWindowedShape(t *testing.T) {
	sim, loc := steadySim(t)
	start := time.Date(2026, 6, 11, 14, 29, 0, 0, loc)
	stop := time.Date(2026, 6, 11, 16, 0, 0, 0, loc)

	rows, err := sim.Answer(BuildCounterSeriesFlux("b", []string{"winefridge"}, start, stop, "30m", "Europe/London"))
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}
	// The first stamp is the RANGE start (14:29), not the grid boundary below it
	// (14:00): aggregateWindow truncates window bounds to the range. Every later
	// stamp is its grid boundary. The caller must therefore resolve row 0 by
	// containment, which is the path the off-grid cases in #27/#29 depend on.
	want := []time.Time{
		start,
		time.Date(2026, 6, 11, 14, 30, 0, 0, loc),
		time.Date(2026, 6, 11, 15, 0, 0, 0, loc),
		time.Date(2026, 6, 11, 15, 30, 0, 0, loc),
	}
	if len(rows) != len(want) {
		t.Fatalf("rows = %d (%+v), want %d — no window is spent as a seed", len(rows), rows, len(want))
	}
	for i, w := range want {
		if !rows[i].Time.Equal(w) {
			t.Errorf("row %d stamped %v, want %v", i, rows[i].Time, w)
		}
	}
	// increase() re-bases at the first reading >= 14:29, so the 14:00 window
	// closes at 14:29 with 0 accumulated, and each later window adds its half
	// hour: 0, 0.5, 1.0, 1.5 at 1 kW.
	for i, w := range []float64{0, 0.5, 1.0, 1.5} {
		if math.Abs(rows[i].Value-w) > 1e-9 {
			t.Errorf("row %d running total = %v, want %v", i, rows[i].Value, w)
		}
	}
}

// A range that opens ON a grid boundary has nothing to truncate: every stamp is
// its own boundary, and the caller's exact-match path does the work.
func TestCounterSimAlignedRangeStampsGridBoundaries(t *testing.T) {
	sim, loc := steadySim(t)
	start := time.Date(2026, 6, 11, 14, 0, 0, 0, loc)
	stop := time.Date(2026, 6, 11, 15, 30, 0, 0, loc)

	rows, err := sim.Answer(BuildCounterSeriesFlux("b", []string{"winefridge"}, start, stop, "30m", "Europe/London"))
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}
	for i, r := range rows {
		if _, m, sec := r.Time.In(loc).Clock(); sec != 0 || (m != 0 && m != 30) {
			t.Errorf("row %d stamped %v, want a 30m grid boundary", i, r.Time.In(loc))
		}
	}
}

// A window the device did not report in is OMITTED rather than emitted as a
// zero, so the caller can carry the running total across a gap instead of
// reading it as a counter reset.
func TestCounterSimOmitsWindowsWithNoReading(t *testing.T) {
	loc := simLondon(t)
	base := time.Date(2026, 6, 11, 12, 0, 0, 0, loc)
	var at []time.Time
	var kwh []float64
	for _, ts := range []time.Time{base, base.Add(90 * time.Minute)} {
		at = append(at, ts)
		kwh = append(kwh, 1000*ts.Sub(base).Hours()/1000)
	}
	sim := NewCounterSim(loc).AddSamples("winefridge", at, kwh)

	rows, err := sim.Answer(BuildCounterSeriesFlux("b", []string{"winefridge"},
		base, base.Add(2*time.Hour), "30m", "Europe/London"))
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}
	// Readings at 12:00 and 13:30 only: the 12:30 and 13:00 windows are absent.
	if len(rows) != 2 {
		t.Fatalf("rows = %+v, want 2 (the two windows holding a reading)", rows)
	}
	if !rows[0].Time.Equal(base) || !rows[1].Time.Equal(base.Add(90*time.Minute)) {
		t.Errorf("rows stamped %v/%v, want 12:00 and 13:30", rows[0].Time, rows[1].Time)
	}
	if math.Abs(rows[1].Value-1.5) > 1e-9 {
		t.Errorf("running total at 13:30 = %v, want 1.5 (90 min at 1 kW since the window opened)", rows[1].Value)
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

	rows, err := sim.Answer(BuildCounterFlux("b", "freezer", from, to))
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
	rows, err := sim.Answer(BuildCounterFlux("b", "winefridge",
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
	// Running totals, so the 25h London day shows up as the RISE across it: the
	// 25th's close minus the 24th's is 25 kWh at 1 kW, not 24.
	var d24, d25 float64
	for _, r := range rows {
		switch r.Time.In(loc).Day() {
		case 24:
			d24 = r.Value
		case 25:
			d25 = r.Value
		}
	}
	if math.Abs(d25-d24-25.0) > 1e-9 {
		t.Errorf("the DST day's rise = %v kWh, want 25 (a 25h London day at 1 kW)", d25-d24)
	}
}
