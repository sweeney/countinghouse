package energy

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/sweeney/countinghouse/internal/config"
	"github.com/sweeney/countinghouse/internal/influx"
)

// ---------------------------------------------------------------------------
// Issue #29: the counter series must be anchored at `from`, not at whatever
// reading the device last managed before it.
//
// The padded difference() design took its zero point from the seed bucket — a
// reading BEFORE the window — and spent the first non-empty window establishing
// it. Neither is a property of the window the caller asked for, so a reading gap
// near `from` moved the answer in either direction without bound.
//
// The invariant these tests defend is one sentence: over any window, for any
// COUNTER-CLASS device, /series total_kwh equals /devices/{id}/energy kwh. Both
// are now increase() anchored at the first reading >= from.
//
// Counter-class is the real scope, not an artefact of the fixtures. A ups_sensor
// has no counter at all: /devices/{id}/energy integrates its power_w while the
// series takes mean power x bucket hours, and two estimators of the same integral
// agree only for evenly-spaced samples. CounterSim answers energy_kwh only, so it
// could not express that case even if the invariant held — the UPS paths are
// covered separately, and their convergence is its own piece of work.
// ---------------------------------------------------------------------------

// gappyCounter builds a steady 1 kW counter sampled every minute over
// [from, to], with the given half-open intervals omitted entirely — a device
// that dropped off the network and came back with its counter still running.
func gappyCounter(loc *time.Location, from, to time.Time, gaps ...[2]time.Time) *influx.CounterSim {
	return addGappyDevice(influx.NewCounterSim(loc), "winefridge", from, to, gaps...)
}

// addGappyDevice registers one more steady-1 kW counter on an existing sim, so a
// test can put a busy device alongside a silent one.
func addGappyDevice(sim *influx.CounterSim, id string, from, to time.Time, gaps ...[2]time.Time) *influx.CounterSim {
	var at []time.Time
	var kwh []float64
	for ts := from; !ts.After(to); ts = ts.Add(time.Minute) {
		skip := false
		for _, g := range gaps {
			if !ts.Before(g[0]) && ts.Before(g[1]) {
				skip = true
			}
		}
		if skip {
			continue
		}
		at = append(at, ts)
		kwh = append(kwh, 1000*ts.Sub(from).Hours()/1000)
	}
	return sim.AddSamples(id, at, kwh)
}

// assertSeriesMatchesScalar is the whole point: the two endpoints must describe
// the same electricity. It returns the series so a caller can make further
// assertions about the shape.
func assertSeriesMatchesScalar(t *testing.T, sim *influx.CounterSim, win Window, ivToken string, loc *time.Location) Series {
	t.Helper()
	iv, ok := lookupInterval(ivToken)
	if !ok {
		t.Fatalf("unknown interval %q", ivToken)
	}
	devices := map[string]config.DeviceConfig{"winefridge": {Class: "continuous_power_device"}}

	resp, err := BuildSeries(context.Background(), &influx.FakeQuerier{QueryFunc: sim.Answer},
		"b", win, iv, GroupByDevice, false, false, devices, testTariff(), nil, loc)
	if err != nil {
		t.Fatalf("BuildSeries: %v", err)
	}
	scalar, _, err := DeviceWindowKWh(context.Background(), &influx.FakeQuerier{QueryFunc: sim.Answer},
		"b", "winefridge", "continuous_power_device", win.Start, win.Stop)
	if err != nil {
		t.Fatalf("DeviceWindowKWh: %v", err)
	}
	if len(resp.Series) != 1 {
		t.Fatalf("series = %d, want 1", len(resp.Series))
	}
	s := resp.Series[0]
	// Per-bucket kWh is rounded to 3dp before summing, so the slack is half a unit
	// in the last place per bucket — but capped: over a long axis that formula
	// alone grows big enough to hide real drift (a 5m interval over a day would
	// allow 0.144 kWh).
	tol := 0.0005 * float64(len(resp.Buckets))
	if tol > 0.01 {
		tol = 0.01
	}
	if math.Abs(s.TotalKWh-scalar) > tol {
		t.Errorf("/series total_kwh = %.4f but /devices/{id}/energy kwh = %.4f over the same window\n"+
			"  delta %+.4f (tolerance %.4f)\n  buckets %v\n  kwh     %v",
			s.TotalKWh, scalar, s.TotalKWh-scalar, tol, len(resp.Buckets), s.KWh)
	}
	return s
}

// Symptom 1: a gap SPANNING the window start. The last reading before the gap
// (14:00) predates `from` (14:29), so the old anchor billed everything since
// 14:00 — 29 minutes of energy from outside the window, +77% on this window.
func TestCounterSeriesGapSpanningWindowStart(t *testing.T) {
	loc := mustLondon(t)
	sim := gappyCounter(loc,
		time.Date(2026, 6, 11, 12, 0, 0, 0, loc),
		time.Date(2026, 6, 11, 18, 0, 0, 0, loc),
		[2]time.Time{time.Date(2026, 6, 11, 14, 0, 0, 0, loc), time.Date(2026, 6, 11, 15, 10, 0, 0, loc)})

	s := assertSeriesMatchesScalar(t, sim, Window{
		Start: time.Date(2026, 6, 11, 14, 29, 0, 0, loc),
		Stop:  time.Date(2026, 6, 11, 16, 43, 0, 0, loc),
		Label: WindowCustom}, "30m", loc)

	// Buckets wholly inside the gap report nothing — not a share of it.
	if s.KWh[0] != 0 || s.KWh[1] != 0 {
		t.Errorf("buckets inside the reading gap = %v, want zeroes", s.KWh[:2])
	}
}

// Symptom 2: a gap spanning the PAD. The pad window is empty, so difference()
// used to consume the first window that had data as its seed and that bucket's
// whole hour vanished (−11%). Note this is a grid-aligned window=today, which
// #27 listed as unaffected — it is unaffected by that bug, not by this one.
func TestCounterSeriesGapSpanningThePadOnAlignedWindow(t *testing.T) {
	loc := mustLondon(t)
	sim := gappyCounter(loc,
		time.Date(2026, 6, 10, 12, 0, 0, 0, loc),
		time.Date(2026, 6, 11, 18, 0, 0, 0, loc),
		[2]time.Time{time.Date(2026, 6, 10, 22, 0, 0, 0, loc), time.Date(2026, 6, 11, 3, 0, 0, 0, loc)})

	s := assertSeriesMatchesScalar(t, sim, Window{
		Start: time.Date(2026, 6, 11, 0, 0, 0, 0, loc),
		Stop:  time.Date(2026, 6, 11, 12, 0, 0, 0, loc),
		Label: WindowToday}, "1h", loc)

	// The device is silent until 03:00, so the first three buckets are zero and
	// the 03:00 bucket carries its real energy rather than being spent as a seed.
	for i := 0; i < 3; i++ {
		if s.KWh[i] != 0 {
			t.Errorf("bucket %d = %v, want 0 (device silent until 03:00)", i, s.KWh[i])
		}
	}
	if s.KWh[3] <= 0 {
		t.Errorf("bucket 3 (03:00) = %v, want > 0 — it must not be consumed as a seed", s.KWh[3])
	}
}

// Symptom 3: no gap at all. Even in the happy path the old seed was the last
// reading BEFORE the window, so every window over-reported by the sliver between
// that reading and `from`. Small, but it is why the two endpoints never agreed
// exactly.
func TestCounterSeriesAlignedWindowNoGapAgreesExactly(t *testing.T) {
	loc := mustLondon(t)
	sim := gappyCounter(loc,
		time.Date(2026, 6, 10, 12, 0, 0, 0, loc),
		time.Date(2026, 6, 11, 18, 0, 0, 0, loc))

	assertSeriesMatchesScalar(t, sim, Window{
		Start: time.Date(2026, 6, 11, 0, 0, 0, 0, loc),
		Stop:  time.Date(2026, 6, 11, 12, 0, 0, 0, loc),
		Label: WindowToday}, "1h", loc)
}

// The off-grid case #27 fixed must stay fixed once the anchor moves.
func TestCounterSeriesOffGridStartStillAgrees(t *testing.T) {
	loc := mustLondon(t)
	sim := gappyCounter(loc,
		time.Date(2026, 6, 11, 12, 0, 0, 0, loc),
		time.Date(2026, 6, 11, 18, 0, 0, 0, loc))

	s := assertSeriesMatchesScalar(t, sim, Window{
		Start: time.Date(2026, 6, 11, 14, 29, 0, 0, loc),
		Stop:  time.Date(2026, 6, 11, 16, 43, 0, 0, loc),
		Label: WindowCustom}, "30m", loc)

	// Bucket 0 still covers only 14:29->14:30, so it bills at most a minute at
	// 1 kW — not the half hour its grid interval spans.
	if s.KWh[0] > 1.0/60+1e-9 {
		t.Errorf("first bucket = %v, want <= %v (14:29->14:30 only)", s.KWh[0], 1.0/60)
	}
	for i := 1; i < len(s.KWh)-1; i++ {
		if math.Abs(s.KWh[i]-0.5) > 1e-9 {
			t.Errorf("interior bucket %d = %v, want 0.5", i, s.KWh[i])
		}
	}
}

// A device that reports nothing at all in the window contributes nothing — and,
// with a busy device beside it in the same response, must not inherit any of its
// running total. demuxCounterTotals keys `closes` per device precisely so the
// carry-forward cannot cross between them; this is what pins that.
func TestCounterSeriesSilentDeviceIsAllZeroBesideABusyOne(t *testing.T) {
	loc := mustLondon(t)
	from := time.Date(2026, 6, 11, 12, 0, 0, 0, loc)

	sim := influx.NewCounterSim(loc)
	addGappyDevice(sim, "winefridge", from, time.Date(2026, 6, 11, 13, 0, 0, 0, loc)) // silent after 13:00
	addGappyDevice(sim, "freezer", from, time.Date(2026, 6, 11, 18, 0, 0, 0, loc))    // reports throughout

	win := Window{
		Start: time.Date(2026, 6, 11, 14, 0, 0, 0, loc),
		Stop:  time.Date(2026, 6, 11, 16, 0, 0, 0, loc),
		Label: WindowCustom,
	}
	iv, _ := lookupInterval("30m")
	devices := map[string]config.DeviceConfig{
		"winefridge": {Class: "continuous_power_device"},
		"freezer":    {Class: "continuous_power_device"},
	}

	resp, err := BuildSeries(context.Background(), &influx.FakeQuerier{QueryFunc: sim.Answer},
		"b", win, iv, GroupByDevice, false, false, devices, testTariff(), nil, loc)
	if err != nil {
		t.Fatalf("BuildSeries: %v", err)
	}
	byKey := map[string]Series{}
	for _, s := range resp.Series {
		byKey[s.Key] = s
	}
	if len(byKey) != 2 {
		t.Fatalf("series = %v, want both devices", resp.Series)
	}

	silent := byKey["winefridge"]
	for i, v := range silent.KWh {
		if v != 0 {
			t.Errorf("silent device bucket %d = %v, want 0 — it reported nothing in the window", i, v)
		}
	}
	if silent.TotalKWh != 0 {
		t.Errorf("silent device total = %v, want 0", silent.TotalKWh)
	}

	// The busy device is unaffected by its silent neighbour. Each bucket is
	// close-to-close, so at a 1-minute cadence bucket 0 runs 14:00->14:29 and the
	// odd minute rolls into the next one — ~0.5 each, and the total is the window.
	busy := byKey["freezer"]
	for i, v := range busy.KWh {
		if v < 0.45 || v > 0.5 {
			t.Errorf("busy device bucket %d = %v, want ~0.5 (30m at 1 kW)", i, v)
		}
	}
	scalar, _, err := DeviceWindowKWh(context.Background(), &influx.FakeQuerier{QueryFunc: sim.Answer},
		"b", "freezer", "continuous_power_device", win.Start, win.Stop)
	if err != nil {
		t.Fatalf("DeviceWindowKWh: %v", err)
	}
	if math.Abs(busy.TotalKWh-scalar) > 0.0005*float64(len(resp.Buckets)) {
		t.Errorf("busy device total = %v, want %v (its own scalar) — a silent neighbour must not perturb it",
			busy.TotalKWh, scalar)
	}
}
