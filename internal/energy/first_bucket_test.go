package energy

import (
	"context"
	"math"
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
	// A steady 1 kW load reporting every minute, from well before the window.
	sim := influx.NewPowerSim(loc).AddSteady("network-ups",
		time.Date(2026, 6, 11, 12, 0, 0, 0, loc),
		time.Date(2026, 6, 11, 18, 0, 0, 0, loc),
		time.Minute, 1000)

	resp, err := BuildSeries(context.Background(), &influx.FakeQuerier{QueryFunc: sim.Answer},
		"statehouse", win, iv, GroupByDevice, false, false, devices, testTariff(), nil, loc)
	if err != nil {
		t.Fatalf("BuildSeries: %v", err)
	}
	ups := resp.Series[0]

	// Bucket 0 covers only 14:29->14:30 = 1 min at 1 kW = 0.0167 kWh. Since issue
	// #32 this clip is the QUERY's: the range starts at win.Start, so the first
	// window is truncated to it and there is no full grid interval to mis-scale.
	if want := round.To((1.0/60)*1000/1000, round.KWhDP); ups.KWh[0] != want {
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

	// What bucketHours still owns is avg_w, which is energy-derived: bucket 0's
	// minute of energy divided by its CLIPPED minute reads the true 1 kW. Divided
	// by the full half-hour grid interval it would read ~33 W — a UPS apparently
	// idling through the very bucket the caller asked about.
	for i, w := range ups.AvgW {
		if math.Abs(w-1000) > 0.1 {
			t.Errorf("ups avg_w[%d] = %v, want 1000 (steady 1 kW; bucket 0 proves the head clip)", i, w)
		}
	}
}

// A UPS is the one metered class whose energy is an ESTIMATE rather than a
// counter reading, so /series and /devices/{id}/energy have to agree on how they
// estimate it. Since issue #32 both integrate power_w; before, the series
// averaged the samples and multiplied by the bucket length.
//
// The sampling here is deliberately UNEVEN, because that is the only thing that
// separates the two estimators — a steady load reported on a regular cadence
// makes a sample mean and a time-weighted integral identical, which is exactly
// why this went unnoticed against the two real UPSs. Bucket 0 sees 1 kW for one
// minute and then 100 W for the rest of the half hour, but only three samples:
// the sample mean reads 400 W across the whole bucket (0.2 kWh) where the true
// time-weighted energy is 0.0575 kWh, a 248% overstatement in one bucket.
//
// Both sides read one PowerSim, so a disagreement is arithmetic rather than
// fixture skew.
//
// The step sits WHOLLY INSIDE bucket 0, and that is load-bearing rather than
// incidental: the two reductions see different neighbours only at a bucket
// boundary, so they agree exactly while the power is constant across each one.
// Move this step to straddle 14:30 and the comparison parts by 0.0075 kWh —
// see influx.TestPowerSimDivergesWhenPowerStepsAcrossABoundary, which pins that
// term and its size. Keep the step inside a bucket when editing this fixture,
// or the failure will look like a code regression when it is the sim's edge
// model.
func TestUPSSeriesTotalAgreesWithDeviceEnergyOnUnevenSampling(t *testing.T) {
	loc := mustLondon(t)
	start := time.Date(2026, 6, 11, 14, 0, 0, 0, loc)
	stop := time.Date(2026, 6, 11, 15, 0, 0, 0, loc)
	win := Window{Start: start, Stop: stop, Label: WindowCustom}
	iv, _ := lookupInterval("30m")
	devices := map[string]config.DeviceConfig{
		"network-ups": {Class: "ups_sensor", DisplayName: "UPS"},
	}

	// Bucket 0: three samples in the first two minutes — 1 kW, then 100 W.
	// Bucket 1: a normal one-minute cadence at 100 W.
	at := []time.Time{start, start.Add(time.Minute), start.Add(2 * time.Minute)}
	w := []float64{1000, 100, 100}
	for ts := start.Add(30 * time.Minute); ts.Before(stop); ts = ts.Add(time.Minute) {
		at = append(at, ts)
		w = append(w, 100)
	}
	sim := influx.NewPowerSim(loc).AddSamples("network-ups", at, w)
	q := &influx.FakeQuerier{QueryFunc: sim.Answer}

	resp, err := BuildSeries(context.Background(), q, "statehouse", win, iv,
		GroupByDevice, false, false, devices, testTariff(), nil, loc)
	if err != nil {
		t.Fatalf("BuildSeries: %v", err)
	}
	scalar, path, err := DeviceWindowKWh(context.Background(), q, "statehouse",
		"network-ups", "ups_sensor", win.Start, win.Stop)
	if err != nil {
		t.Fatalf("DeviceWindowKWh: %v", err)
	}
	if path != PathIntegral {
		t.Fatalf("path = %q, want %q", path, PathIntegral)
	}

	total := resp.Series[0].TotalKWh
	// Per-bucket kWh is rounded to 3dp before summing; the cap keeps a long axis
	// from hiding real drift, as in counter_anchor_test.go.
	tol := 0.0005 * float64(len(resp.Buckets))
	if tol > 0.01 {
		tol = 0.01
	}
	if math.Abs(total-scalar) > tol {
		t.Errorf("/series total_kwh = %.4f but /devices/network-ups/energy kwh = %.4f\n"+
			"  delta %+.4f (tolerance %.4f)\n  kwh %v",
			total, scalar, total-scalar, tol, resp.Series[0].KWh)
	}

	// Pin the fixture itself, so a future change that quietly made the sampling
	// even again would fail here rather than making the agreement above trivial.
	if want := 0.0575; math.Abs(resp.Series[0].KWh[0]-round.To(want, round.KWhDP)) > 1e-9 {
		t.Errorf("bucket 0 = %v kWh, want %v — the sample mean would say 0.2",
			resp.Series[0].KWh[0], want)
	}
}

// The residual this change does NOT close, pinned so it cannot drift unnoticed:
// a bucket the UPS reported nothing in has nothing to integrate and publishes 0,
// while the whole-window integral bridges the outage. The two therefore part
// company by the bridged energy, and the series is the low one.
//
// Fixing that needs the bracketing samples, which a per-bucket query cannot see.
// It is recorded in the docs as a known limit rather than papered over.
func TestUPSTotalOutageBucketReadsZeroAndUndercutsTheScalar(t *testing.T) {
	loc := mustLondon(t)
	start := time.Date(2026, 6, 11, 14, 0, 0, 0, loc)
	stop := time.Date(2026, 6, 11, 16, 0, 0, 0, loc)
	win := Window{Start: start, Stop: stop, Label: WindowCustom}
	iv, _ := lookupInterval("30m")
	devices := map[string]config.DeviceConfig{
		"network-ups": {Class: "ups_sensor", DisplayName: "UPS"},
	}
	// Silent for the whole 14:30 bucket.
	sim := influx.NewPowerSim(loc).AddSteadyWithGaps("network-ups", start, stop, time.Minute, 1000,
		[2]time.Time{time.Date(2026, 6, 11, 14, 30, 0, 0, loc), time.Date(2026, 6, 11, 15, 0, 0, 0, loc)})
	q := &influx.FakeQuerier{QueryFunc: sim.Answer}

	resp, err := BuildSeries(context.Background(), q, "statehouse", win, iv,
		GroupByDevice, false, false, devices, testTariff(), nil, loc)
	if err != nil {
		t.Fatalf("BuildSeries: %v", err)
	}
	ups := resp.Series[0]
	if ups.KWh[1] != 0 {
		t.Errorf("outage bucket kwh = %v, want 0 — nothing was reported to integrate", ups.KWh[1])
	}
	// And avg_w follows the energy rather than contradicting it.
	if ups.AvgW[1] != 0 {
		t.Errorf("outage bucket avg_w = %v, want 0 to match its own kwh", ups.AvgW[1])
	}

	scalar, _, err := DeviceWindowKWh(context.Background(), q, "statehouse",
		"network-ups", "ups_sensor", start, stop)
	if err != nil {
		t.Fatalf("DeviceWindowKWh: %v", err)
	}
	// The scalar bridges the outage, so it is HIGHER by about the half hour lost.
	if gap := scalar - ups.TotalKWh; math.Abs(gap-0.5) > 0.02 {
		t.Errorf("scalar %.4f - series %.4f = %.4f, want ~0.5 (the bridged outage)", scalar, ups.TotalKWh, gap)
	}

	// The device is NOT stale: it reported for three of the four buckets. The
	// staleness signal is for a device that said nothing at all.
	if ups.AvgW[0] == 0 {
		t.Errorf("avg_w[0] = 0, want the real load — only the outage bucket is empty")
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
