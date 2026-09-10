package energy

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/sweeney/countinghouse/internal/config"
	"github.com/sweeney/countinghouse/internal/influx"
	"github.com/sweeney/countinghouse/internal/round"
)

// ---------------------------------------------------------------------------
// Issue #27: the FIRST bucket must be clipped to win.Start, as the LAST one is
// already clipped to win.Stop.
//
// The canonical axis snaps DOWN to the interval grid (issue #1) — that is
// deliberate and stays. What was missing is the other half: nothing clipped the
// first bucket's VALUE to the window it claims to describe, so /series billed
// energy from before `from` and disagreed with /devices/{id}/energy over the
// very same window.
// ---------------------------------------------------------------------------

// ---- bucketHours (pure) ----------------------------------------------------

// TestBucketHoursClipsFirstBucketToWindowStart is the issue #27 unit
// reproduction. Window 14:29 → 16:43 on a 30m grid: the axis snaps to 14:00
// (correct), but bucket 0 only actually covers 14:29→14:30 = 1 minute.
func TestBucketHoursClipsFirstBucketToWindowStart(t *testing.T) {
	loc := mustLondon(t)
	win := Window{
		Start: time.Date(2026, 6, 11, 14, 29, 0, 0, loc),
		Stop:  time.Date(2026, 6, 11, 16, 43, 0, 0, loc),
		Label: WindowCustom,
	}
	iv, _ := lookupInterval("30m")

	hrs := bucketHours(BucketStarts(win, iv, loc), win.Start, win.Stop)

	// The tail has always been clipped: 16:30 → 16:43 = 13 min.
	if got, want := hrs[len(hrs)-1], 13.0/60; math.Abs(got-want) > 1e-9 {
		t.Errorf("last bucket hours = %v, want %v", got, want)
	}
	// The head must be clipped too: 14:29 → 14:30 = 1 min.
	if got, want := hrs[0], 1.0/60; math.Abs(got-want) > 1e-9 {
		t.Errorf("first bucket hours = %v, want %v (1 min: 14:29->14:30)", got, want)
	}
	// Interior buckets are untouched full intervals.
	for i := 1; i < len(hrs)-1; i++ {
		if math.Abs(hrs[i]-0.5) > 1e-9 {
			t.Errorf("interior bucket %d hours = %v, want 0.5", i, hrs[i])
		}
	}
}

// TestBucketHoursGridAlignedStartIsUnchanged guards the fix against
// over-reaching: when `from` is already on the grid (every today/week/month/<N>d
// window) the first bucket is a FULL interval and must stay one.
func TestBucketHoursGridAlignedStartIsUnchanged(t *testing.T) {
	loc := mustLondon(t)
	win := Window{
		Start: time.Date(2026, 6, 11, 0, 0, 0, 0, loc),
		Stop:  time.Date(2026, 6, 11, 2, 30, 0, 0, loc),
		Label: WindowToday,
	}
	iv, _ := lookupInterval("1h")

	hrs := bucketHours(BucketStarts(win, iv, loc), win.Start, win.Stop)
	want := []float64{1, 1, 0.5}
	if len(hrs) != len(want) {
		t.Fatalf("hours = %v, want len %d", hrs, len(want))
	}
	for i := range want {
		if math.Abs(hrs[i]-want[i]) > 1e-9 {
			t.Errorf("bucket %d hours = %v, want %v", i, hrs[i], want[i])
		}
	}
}

// TestBucketHoursSingleBucketClippedBothEnds covers the degenerate case where
// the whole window lives inside ONE grid bucket (14:05->14:20 sits entirely in
// the 14:00 half-hour): head and tail clipping apply to the same bucket, which
// must then report exactly the window's own length.
func TestBucketHoursSingleBucketClippedBothEnds(t *testing.T) {
	loc := mustLondon(t)
	win := Window{
		Start: time.Date(2026, 6, 11, 14, 5, 0, 0, loc),
		Stop:  time.Date(2026, 6, 11, 14, 20, 0, 0, loc),
		Label: WindowCustom,
	}
	iv, _ := lookupInterval("30m")

	hrs := bucketHours(BucketStarts(win, iv, loc), win.Start, win.Stop)
	if len(hrs) != 1 {
		t.Fatalf("hours = %v, want a single bucket", hrs)
	}
	if want := 15.0 / 60; math.Abs(hrs[0]-want) > 1e-9 {
		t.Errorf("sole bucket hours = %v, want %v (14:05->14:20)", hrs[0], want)
	}
}

// TestBucketHoursCalendarDayClipsHead exercises the 1d calendar axis, whose
// bucket 0 is local midnight: a custom window starting mid-afternoon must bill
// only the rest of that day, not the whole of it.
func TestBucketHoursCalendarDayClipsHead(t *testing.T) {
	loc := mustLondon(t)
	win := Window{
		Start: time.Date(2026, 6, 11, 18, 0, 0, 0, loc),
		Stop:  time.Date(2026, 6, 13, 6, 0, 0, 0, loc),
		Label: WindowCustom,
	}
	iv, _ := lookupInterval("1d")

	hrs := bucketHours(BucketStarts(win, iv, loc), win.Start, win.Stop)
	want := []float64{6, 24, 6} // 18:00->24:00, a full day, 00:00->06:00
	if len(hrs) != len(want) {
		t.Fatalf("hours = %v, want len %d", hrs, len(want))
	}
	for i := range want {
		if math.Abs(hrs[i]-want[i]) > 1e-9 {
			t.Errorf("bucket %d hours = %v, want %v", i, hrs[i], want[i])
		}
	}
}

// TestWindowStartInsideFirstBucketChangesEnergy is the behavioural statement of
// the bug: for a steady load, moving the window start LATER inside the first
// bucket must strictly reduce the billed energy. Before the fix all four starts
// returned an identical figure.
func TestWindowStartInsideFirstBucketChangesEnergy(t *testing.T) {
	loc := mustLondon(t)
	stop := time.Date(2026, 6, 11, 16, 0, 0, 0, loc)
	iv, _ := lookupInterval("30m")

	const steadyWatts = 1000.0
	total := func(startMin int) float64 {
		win := Window{
			Start: time.Date(2026, 6, 11, 14, startMin, 0, 0, loc),
			Stop:  stop, Label: WindowCustom,
		}
		var kwh float64
		for _, h := range bucketHours(BucketStarts(win, iv, loc), win.Start, win.Stop) {
			kwh += steadyWatts * h / 1000
		}
		return kwh
	}

	prev := total(0)
	for _, m := range []int{1, 17, 29} {
		got := total(m)
		if got >= prev {
			t.Errorf("energy from 14:%02d (%.4f) must be LESS than the previous start (%.4f); "+
				"the minutes before the window start are being billed", m, got, prev)
		}
		prev = got
	}
	// And the figure must be exactly the window's own length × the load:
	// 14:29->16:00 is 91 minutes, not the 120 the unclipped head reported.
	if want := 91.0 / 60; math.Abs(total(29)-want) > 1e-9 {
		t.Errorf("energy from 14:29 = %.6f kWh, want %.6f (14:29->16:00 at 1 kW)", total(29), want)
	}
}

// ---- BuildSeries: UPS / power path -----------------------------------------

// TestBuildSeriesUPSClipsFirstBucketToWindowStart is the orchestrator-level
// reproduction for the integral path. UPS energy is mean(power_w) ×
// bucket_hours, so an unclipped bucket 0 takes a mean measured over 1 minute of
// samples and scales it across the full 30-minute grid interval.
func TestBuildSeriesUPSClipsFirstBucketToWindowStart(t *testing.T) {
	loc := mustLondon(t)
	win := Window{
		Start: time.Date(2026, 6, 11, 14, 29, 0, 0, loc),
		Stop:  time.Date(2026, 6, 11, 16, 0, 0, 0, loc),
		Label: WindowCustom,
	}
	iv, _ := lookupInterval("30m")
	buckets := BucketStarts(win, iv, loc)
	if len(buckets) != 4 {
		t.Fatalf("buckets = %v, want 4 (14:00,14:30,15:00,15:30)", buckets)
	}

	devices := map[string]config.DeviceConfig{
		"network-ups": {Class: "ups_sensor", DisplayName: "UPS"},
	}
	// A steady 1 kW load: every bucket's mean is 1000 W.
	var powerRows []influx.Row
	for _, b := range buckets {
		powerRows = append(powerRows, influx.Row{DeviceID: "network-ups", Field: "power_w", Value: 1000, Time: b})
	}
	q := &influx.FakeQuerier{QueryFunc: func(flux string) ([]influx.Row, error) {
		if strings.Contains(flux, "power_w") {
			return powerRows, nil
		}
		return nil, nil
	}}

	resp, err := BuildSeries(context.Background(), q, "statehouse", win, iv, GroupByDevice, false, false, devices, testTariff(), nil, loc)
	if err != nil {
		t.Fatalf("BuildSeries: %v", err)
	}
	ups := resp.Series[0]

	// Bucket 0 covers only 14:29->14:30 = 1 min at 1 kW = 0.0167 kWh.
	if want := round.To(1000*(1.0/60)/1000, round.KWhDP); ups.KWh[0] != want {
		t.Errorf("ups kwh[0] = %v, want %v (only 14:29->14:30 is inside the window)", ups.KWh[0], want)
	}
	for i := 1; i < len(buckets); i++ {
		if ups.KWh[i] != 0.5 {
			t.Errorf("ups kwh[%d] = %v, want 0.5 (full 30m bucket at 1 kW)", i, ups.KWh[i])
		}
	}
	// Total is the window's true energy: 1h31m at 1 kW.
	if want := round.To(0.5*3+1.0/60, round.KWhDP); math.Abs(ups.TotalKWh-want) > 1.5e-3 {
		t.Errorf("ups total_kwh = %v, want ~%v (14:29->16:00 at 1 kW)", ups.TotalKWh, want)
	}
}

// The counter path's own first-bucket behaviour is covered by
// counter_anchor_test.go: anchoring the series at `from` (issue #29) makes
// bucket 0 correct by construction and removed the separate head query this file
// used to exercise.

// ---- the invariant that would have caught this -----------------------------

// TestSeriesTotalAgreesWithDeviceEnergyOffGridWindow is the invariant issue #27
// asks for, and the one that would have caught the bug: over the SAME window,
// /series total_kwh and /devices/{id}/energy kwh must describe the same
// electricity. Both are driven here off one simulated counter, so any
// disagreement is arithmetic, not fixture skew.
//
// Before the fix the series total was a full grid interval too high — ~29
// minutes of a 2h14m window, ~22% — because bucket 0 reported 14:00->14:30 for a
// window that opens at 14:29.
func TestSeriesTotalAgreesWithDeviceEnergyOffGridWindow(t *testing.T) {
	loc := mustLondon(t)
	sim := influx.NewCounterSim(loc).AddSteady("winefridge",
		time.Date(2026, 6, 11, 12, 0, 0, 0, loc),
		time.Date(2026, 6, 11, 18, 0, 0, 0, loc),
		time.Minute, 1000)

	win := Window{
		Start: time.Date(2026, 6, 11, 14, 29, 0, 0, loc),
		Stop:  time.Date(2026, 6, 11, 16, 43, 0, 0, loc),
		Label: WindowCustom,
	}
	iv, _ := lookupInterval("30m")
	devices := map[string]config.DeviceConfig{
		"winefridge": {Class: "continuous_power_device"},
	}
	q := &influx.FakeQuerier{QueryFunc: sim.Answer}
	ctx := context.Background()

	scalar, _, err := DeviceWindowKWh(ctx, q, "statehouse", "winefridge", "continuous_power_device", win.Start, win.Stop)
	if err != nil {
		t.Fatalf("DeviceWindowKWh: %v", err)
	}
	resp, err := BuildSeries(ctx, q, "statehouse", win, iv, GroupByDevice, false, false, devices, testTariff(), nil, loc)
	if err != nil {
		t.Fatalf("BuildSeries: %v", err)
	}
	if len(resp.Series) != 1 {
		t.Fatalf("series = %d, want 1", len(resp.Series))
	}
	total := resp.Series[0].TotalKWh

	// Per-bucket kWh is rounded to 3dp before summing, so allow half a unit in
	// the last place per bucket and no more.
	tol := 0.0005 * float64(len(resp.Buckets))
	if math.Abs(total-scalar) > tol {
		t.Errorf("/series total_kwh = %.4f but /devices/{id}/energy kwh = %.4f "+
			"over the same window (delta %.4f, tolerance %.4f); the two endpoints must agree",
			total, scalar, total-scalar, tol)
	}
	// And both must be the physically right answer: 1 kW for 14:29->16:42
	// (the last sample strictly inside the window, at a 1-minute cadence).
	if want := 133.0 / 60; math.Abs(total-want) > tol {
		t.Errorf("/series total_kwh = %.4f, want ~%.4f (1 kW over 14:29->16:42)", total, want)
	}
}

// TestSeriesTotalFallsAsTheWindowStartMovesLater is the end-to-end form of the
// symptom the issue reports: holding `to` fixed and walking `from` later through
// the first grid bucket returned a byte-identical total every time.
func TestSeriesTotalFallsAsTheWindowStartMovesLater(t *testing.T) {
	loc := mustLondon(t)
	sim := influx.NewCounterSim(loc).AddSteady("winefridge",
		time.Date(2026, 6, 11, 12, 0, 0, 0, loc),
		time.Date(2026, 6, 11, 18, 0, 0, 0, loc),
		time.Minute, 1000)
	iv, _ := lookupInterval("30m")
	devices := map[string]config.DeviceConfig{
		"winefridge": {Class: "continuous_power_device"},
	}

	totalFrom := func(min int) float64 {
		win := Window{
			Start: time.Date(2026, 6, 11, 14, min, 0, 0, loc),
			Stop:  time.Date(2026, 6, 11, 16, 43, 0, 0, loc),
			Label: WindowCustom,
		}
		q := &influx.FakeQuerier{QueryFunc: sim.Answer}
		resp, err := BuildSeries(context.Background(), q, "statehouse", win, iv, GroupByDevice, false, false, devices, testTariff(), nil, loc)
		if err != nil {
			t.Fatalf("BuildSeries: %v", err)
		}
		return resp.Series[0].TotalKWh
	}

	prev := totalFrom(0)
	for _, m := range []int{1, 17, 29} {
		got := totalFrom(m)
		if got >= prev {
			t.Errorf("total_kwh from 14:%02d = %.4f, must be strictly less than the earlier start's %.4f; "+
				"energy from before `from` is being billed", m, got, prev)
		}
		prev = got
	}
}
