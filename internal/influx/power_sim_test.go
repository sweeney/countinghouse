package influx

import (
	"math"
	"testing"
	"time"
)

// PowerSim is only worth trusting if it parses what the REAL builders emit, so
// every case here drives it through BuildIntegralFlux,
// BuildPowerIntegralSeriesFlux or BuildPowerMeanSeriesFlux rather than through a
// hand-written flux string. A change to a query shape then breaks these loudly
// instead of silently teaching the sim to answer a query nobody sends.

func steadyPowerSim(t *testing.T) (*PowerSim, *time.Location) {
	t.Helper()
	loc, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Fatal(err)
	}
	// 1 kW, sampled every minute from 12:00 to 18:00 — a UPS's steady load at a
	// realistic cadence.
	return NewPowerSim(loc).AddSteady("network-ups",
		time.Date(2026, 6, 11, 12, 0, 0, 0, loc),
		time.Date(2026, 6, 11, 18, 0, 0, 0, loc),
		time.Minute, 1000), loc
}

func TestPowerSimWholeWindowIntegral(t *testing.T) {
	sim, loc := steadyPowerSim(t)
	start := time.Date(2026, 6, 11, 14, 0, 0, 0, loc)
	stop := time.Date(2026, 6, 11, 16, 0, 0, 0, loc)

	rows, err := sim.Answer(BuildIntegralFlux("b", "network-ups", start, stop))
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1 (integral reduces to one row per device)", len(rows))
	}
	// 2h at 1 kW, already converted from W.h to kWh by the builder's map().
	if math.Abs(rows[0].Value-2.0) > 1e-9 {
		t.Errorf("kwh = %v, want 2.0 (2h at 1 kW)", rows[0].Value)
	}
}

// The energy series is per-bucket kWh, not watts: the caller folds it with the
// identity, so anything else here would be scaled twice.
func TestPowerSimIntegralSeriesIsKWhPerBucket(t *testing.T) {
	sim, loc := steadyPowerSim(t)
	start := time.Date(2026, 6, 11, 14, 0, 0, 0, loc)
	stop := time.Date(2026, 6, 11, 16, 0, 0, 0, loc)

	rows, err := sim.Answer(BuildPowerIntegralSeriesFlux("b", []string{"network-ups"}, start, stop, "30m", "Europe/London"))
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}
	if len(rows) != 4 {
		t.Fatalf("rows = %d, want 4 (30m buckets over 2h)", len(rows))
	}
	for i, r := range rows {
		if math.Abs(r.Value-0.5) > 1e-9 {
			t.Errorf("bucket %d = %v kWh, want 0.5 (30m at 1 kW)", i, r.Value)
		}
	}
}

// The headline property of issue #32's fix: both endpoints now estimate the same
// integral, so the buckets sum to the whole-window reduction.
//
// The load is steady on purpose, but the property doing the work is narrower
// than steadiness — it is that the power is constant ACROSS EACH BUCKET
// BOUNDARY, which is the only place the two reductions see different
// neighbours. See TestPowerSimDivergesWhenPowerStepsAcrossABoundary for the
// case that separates them, and the PowerSim type comment for why the
// distinction matters.
func TestPowerSimBucketsSumToWholeWindow(t *testing.T) {
	sim, loc := steadyPowerSim(t)
	start := time.Date(2026, 6, 11, 14, 29, 0, 0, loc) // deliberately off-grid
	stop := time.Date(2026, 6, 11, 16, 43, 0, 0, loc)

	whole, err := sim.Answer(BuildIntegralFlux("b", "network-ups", start, stop))
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}
	bucketed, err := sim.Answer(BuildPowerIntegralSeriesFlux("b", []string{"network-ups"}, start, stop, "30m", "Europe/London"))
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}
	var sum float64
	for _, r := range bucketed {
		sum += r.Value
	}
	if math.Abs(sum-whole[0].Value) > 1e-9 {
		t.Errorf("buckets sum to %v but the whole window reduces to %v", sum, whole[0].Value)
	}
}

// An off-grid range truncates its first window, so that row is stamped at the
// RANGE start rather than at the grid boundary below it — the row the energy
// layer must resolve by containment.
func TestPowerSimTruncatesFirstWindowStamp(t *testing.T) {
	sim, loc := steadyPowerSim(t)
	start := time.Date(2026, 6, 11, 14, 29, 0, 0, loc)
	stop := time.Date(2026, 6, 11, 16, 0, 0, 0, loc)

	rows, err := sim.Answer(BuildPowerIntegralSeriesFlux("b", []string{"network-ups"}, start, stop, "30m", "Europe/London"))
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("no rows")
	}
	if !rows[0].Time.Equal(start) {
		t.Errorf("first row stamped %v, want the range start %v", rows[0].Time, start)
	}
	// And it is worth only the minute that is actually inside the window.
	if want := 1.0 / 60; math.Abs(rows[0].Value-want) > 1e-9 {
		t.Errorf("first bucket = %v kWh, want %v (14:29->14:30 at 1 kW)", rows[0].Value, want)
	}
}

// createEmpty: false — a window the device did not report in is OMITTED from the
// energy series, so the caller can tell "drew nothing" from "said nothing".
func TestPowerSimIntegralOmitsWindowsWithNoSample(t *testing.T) {
	loc, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 6, 11, 14, 0, 0, 0, loc)
	stop := time.Date(2026, 6, 11, 16, 0, 0, 0, loc)
	sim := NewPowerSim(loc).AddSteadyWithGaps("network-ups", start, stop, time.Minute, 1000,
		[2]time.Time{time.Date(2026, 6, 11, 14, 30, 0, 0, loc), time.Date(2026, 6, 11, 15, 0, 0, 0, loc)})

	rows, err := sim.Answer(BuildPowerIntegralSeriesFlux("b", []string{"network-ups"}, start, stop, "30m", "Europe/London"))
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3 — the 14:30 window has no sample and must be omitted, not zeroed", len(rows))
	}
	for _, r := range rows {
		if r.Time.Equal(time.Date(2026, 6, 11, 14, 30, 0, 0, loc)) {
			t.Errorf("the empty 14:30 window was emitted: %+v", r)
		}
	}
}

// createEmpty: true on the MEAN query, by contrast, emits the empty window — as
// a null. That is the row that used to decode to 0 W and read as a device
// drawing nothing (issue #32).
func TestPowerSimMeanEmitsNullForAnEmptyWindow(t *testing.T) {
	loc, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 6, 11, 14, 0, 0, 0, loc)
	stop := time.Date(2026, 6, 11, 16, 0, 0, 0, loc)
	sim := NewPowerSim(loc).AddSteadyWithGaps("winefridge", start, stop, time.Minute, 52,
		[2]time.Time{time.Date(2026, 6, 11, 14, 30, 0, 0, loc), time.Date(2026, 6, 11, 15, 0, 0, 0, loc)})

	rows, err := sim.Answer(BuildPowerMeanSeriesFlux("b", []string{"winefridge"}, start, stop, "30m", "Europe/London"))
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}
	if len(rows) != 4 {
		t.Fatalf("rows = %d, want 4 — createEmpty keeps the axis dense", len(rows))
	}
	empty := rows[1]
	if !empty.Null {
		t.Errorf("the 14:30 window is %+v, want Null — it has nothing to average", empty)
	}
	for i, r := range rows {
		if i == 1 {
			continue
		}
		if r.Null || math.Abs(r.Value-52) > 1e-9 {
			t.Errorf("window %d = %+v, want a real mean of 52 W", i, r)
		}
	}
}

// The mean is a SAMPLE mean and the integral is TIME-weighted, which is the whole
// reason the two endpoints disagreed. Samples bunched into the first tenth of a
// bucket must therefore reduce differently: the mean reads the bunched average
// flat across the bucket, the integral holds the last value for the rest of it.
func TestPowerSimMeanAndIntegralDifferOnUnevenSampling(t *testing.T) {
	loc, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 6, 11, 14, 0, 0, 0, loc)
	stop := time.Date(2026, 6, 11, 14, 30, 0, 0, loc)

	// Three samples in the first three minutes: 1000 W, then 100 W for the rest.
	at := []time.Time{start, start.Add(time.Minute), start.Add(2 * time.Minute)}
	sim := NewPowerSim(loc).AddSamples("network-ups", at, []float64{1000, 100, 100})

	meanRows, err := sim.Answer(BuildPowerMeanSeriesFlux("b", []string{"network-ups"}, start, stop, "30m", "Europe/London"))
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}
	intRows, err := sim.Answer(BuildPowerIntegralSeriesFlux("b", []string{"network-ups"}, start, stop, "30m", "Europe/London"))
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}

	// Sample mean: (1000+100+100)/3 = 400 W. Over half an hour that is 0.2 kWh —
	// the number the old series published.
	if got := meanRows[0].Value; math.Abs(got-400) > 1e-9 {
		t.Fatalf("sample mean = %v W, want 400", got)
	}
	oldWay := meanRows[0].Value * 0.5 / 1000

	// Time-weighted: 550 W for a minute, 100 W for a minute, then 100 W held for
	// the remaining 28 — 0.0575 kWh, well under a third of the mean's estimate.
	want := (550*1.0 + 100*1.0 + 100*28.0) / 60 / 1000
	if got := intRows[0].Value; math.Abs(got-want) > 1e-9 {
		t.Fatalf("integral = %v kWh, want %v", got, want)
	}
	if oldWay <= intRows[0].Value*2 {
		t.Errorf("the two estimators should diverge sharply here: mean-derived %v vs integral %v", oldWay, intRows[0].Value)
	}
}

// The exact case the agreement test above does NOT cover, pinned so the size of
// the gap stays visible and so a future edit to a fixture cannot quietly turn a
// sim-model artefact into what looks like a code regression.
//
// It sweeps the sample grid ACROSS the boundary rather than testing one
// alignment, because the term depends on where the readings fall either side of
// it and not merely on the gap between them. With a = boundary - last reading
// before, b = first reading at/after - boundary, the divergence is
//
//	(P1 - P2) * (a - b) / 2
//
// which is maximal when a reading lands on the boundary (b == 0), ZERO when the
// boundary bisects the gap, and NEGATIVE when the next reading is further off
// than the previous one. Pinning only the b == 0 alignment would make the
// fixture load-bearing in exactly the way the agreement test's comment warns
// against — an offset grid would then fail against a true delta of zero and
// read as a code regression.
//
// This is a statement about THIS SIM's edge model, not about Flux — under a
// bound-interpolating integral the sum would telescope exactly. Either way the
// magnitude is bounded by |P1-P2|*(a+b)/2, so its size is set by how violently
// the load moves and how slowly the device reports.
func TestPowerSimDivergesWhenPowerStepsAcrossABoundary(t *testing.T) {
	loc, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 6, 11, 14, 0, 0, 0, loc)
	stop := time.Date(2026, 6, 11, 15, 0, 0, 0, loc)
	boundary := start.Add(30 * time.Minute)
	const p1, p2 = 1000.0, 100.0
	const cadence = time.Minute

	for _, offset := range []time.Duration{0, 15 * time.Second, 30 * time.Second, 59 * time.Second} {
		var at []time.Time
		var w []float64
		for ts := start.Add(offset); ts.Before(stop); ts = ts.Add(cadence) {
			at = append(at, ts)
			if ts.Before(boundary) {
				w = append(w, p1)
			} else {
				w = append(w, p2)
			}
		}
		sim := NewPowerSim(loc).AddSamples("network-ups", at, w)

		whole, err := sim.Answer(BuildIntegralFlux("b", "network-ups", start, stop))
		if err != nil {
			t.Fatalf("offset %v: Answer: %v", offset, err)
		}
		bucketed, err := sim.Answer(BuildPowerIntegralSeriesFlux("b", []string{"network-ups"}, start, stop, "30m", "Europe/London"))
		if err != nil {
			t.Fatalf("offset %v: Answer: %v", offset, err)
		}
		var sum float64
		for _, r := range bucketed {
			sum += r.Value
		}

		// The readings either side of the boundary, from the fixture itself.
		var beforeGap, afterGap time.Duration
		for _, ts := range at {
			if ts.Before(boundary) {
				beforeGap = boundary.Sub(ts)
				continue
			}
			afterGap = ts.Sub(boundary)
			break
		}
		want := (p1 - p2) * (beforeGap - afterGap).Hours() / 2 / 1000

		if got := sum - whole[0].Value; math.Abs(got-want) > 1e-9 {
			t.Errorf("offset %v (a=%v b=%v): series %.4f - scalar %.4f = %+.6f, want %+.6f",
				offset, beforeGap, afterGap, sum, whole[0].Value, got, want)
		}
	}
}

// The magnitude is what the docs quote, so pin the bound rather than the
// adjective: maximal when a reading lands on the boundary, and the reason the
// term is invisible for the real UPSs is the size of their steps, not the
// arithmetic.
func TestPowerSimBoundaryTermBound(t *testing.T) {
	bound := func(stepW float64, gap time.Duration) float64 {
		return stepW * gap.Hours() / 2 / 1000
	}
	// A violent step at a slow cadence rounds into the published 3dp...
	if got := bound(1000, 30*time.Second); got < 0.001 {
		t.Errorf("a 1 kW step at a 30s cadence bounds at %v kWh; the docs claim it is visible at 3dp", got)
	}
	// ...while a UPS's own moves do not.
	if got := bound(50, 30*time.Second); got >= 0.0005 {
		t.Errorf("a 50 W step at a 30s cadence bounds at %v kWh, which would round into the published 3dp", got)
	}
}

func TestPowerSimFiltersDevicesAndField(t *testing.T) {
	sim, loc := steadyPowerSim(t)
	sim.AddSteady("office-ups",
		time.Date(2026, 6, 11, 12, 0, 0, 0, loc),
		time.Date(2026, 6, 11, 18, 0, 0, 0, loc),
		time.Minute, 40)
	start := time.Date(2026, 6, 11, 14, 0, 0, 0, loc)
	stop := time.Date(2026, 6, 11, 15, 0, 0, 0, loc)

	rows, err := sim.Answer(BuildPowerIntegralSeriesFlux("b", []string{"office-ups"}, start, stop, "1h", "Europe/London"))
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}
	for _, r := range rows {
		if r.DeviceID != "office-ups" {
			t.Errorf("row for %q, want only office-ups", r.DeviceID)
		}
	}

	// A counter query is not this sim's field at all.
	counter, err := sim.Answer(BuildCounterSeriesFlux("b", []string{"network-ups"}, start, stop, "1h", "Europe/London"))
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}
	if len(counter) != 0 {
		t.Errorf("energy_kwh query answered with %d rows, want none", len(counter))
	}
}

func TestPowerSimEmptyRangeYieldsNoIntegralRow(t *testing.T) {
	sim, loc := steadyPowerSim(t)
	start := time.Date(2026, 6, 12, 2, 0, 0, 0, loc) // after every sample
	stop := time.Date(2026, 6, 12, 3, 0, 0, 0, loc)

	rows, err := sim.Answer(BuildIntegralFlux("b", "network-ups", start, stop))
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("rows = %v, want none — an empty range has no integral, not an integral of zero", rows)
	}
}

func TestPowerSimRejectsUnknownInterval(t *testing.T) {
	sim, loc := steadyPowerSim(t)
	flux := BuildPowerIntegralSeriesFlux("b", []string{"network-ups"},
		time.Date(2026, 6, 11, 14, 0, 0, 0, loc),
		time.Date(2026, 6, 11, 15, 0, 0, 0, loc), "7m", "Europe/London")
	if _, err := sim.Answer(flux); err == nil {
		t.Error("an interval outside the allowed set must error, not bucket at the wrong width")
	}
}

func TestPowerSimAddSamplesRejectsMismatchedLengths(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("mismatched slice lengths must panic at the call site")
		}
	}()
	NewPowerSim(time.UTC).AddSamples("network-ups", []time.Time{time.Now()}, nil)
}
