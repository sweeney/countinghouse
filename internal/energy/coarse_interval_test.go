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
// Costing a bucket coarser than the price grid.
//
// A half-hourly tariff has 48 prices a day. `/series` does not ask for 48 buckets:
// DefaultInterval gives 1h for window=today and 1d for week/month, and only /bill
// pins 30m via CostingInterval(). Pricing a bucket at `RateAt(bucket start)` is exact
// when the axes line up and silently wrong otherwise — a 1d bucket gets the 00:00
// slot's rate applied to the whole day.
//
// This is the PR's opening bug one layer up. Sharing CostBuckets was not enough: a
// shared function handed an axis coarser than the price grid still returns a confident
// wrong number, and nothing detected that it had happened. Measured on the branch's
// own recorded day, same energy and pricer: 30m £0.3759, 1h £0.3731 (−0.7%), 1d
// £0.5398 (+43.6%) — so /series?window=month and /bill?window=month disagreed by 44%.
//
// These are INTEGRATION tests through BuildSeries with a counter simulator, not unit
// tests on the assembly helper, because the defect is in which axis gets chosen — and
// the helper correctly prices whatever axis it is handed. The first version of this
// file tested the helper and so could not see the bug at all.
// ---------------------------------------------------------------------------

// ciDay is the UTC day these tests run over.
var ciDay = time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)

// ciPricer returns a 48-slot pricer for ciDay whose shape makes a first-slot error
// loud: cheap overnight, dear evening, with the 00:00 slot the cheapest of all. A
// flat or gently-varying day would hide the bug, which is the point of choosing this
// shape rather than a realistic one.
func ciPricer() (Pricer, []float64) {
	pence := make([]float64, 48)
	for i := range pence {
		switch {
		case i < 12: // 00:00–06:00, cheap
			pence[i] = 5
		case i < 32: // 06:00–16:00
			pence[i] = 25
		default: // 16:00–24:00, peak
			pence[i] = 60
		}
	}
	rates := make([]*float64, len(pence))
	for i := range pence {
		v := pence[i]
		rates[i] = &v
	}
	return slotRates{start: ciDay, rates: rates}, pence
}

// exactSlotSim puts EXACTLY kwhPerSlot in each of `slots` half hours from start.
//
// Cumulative readings on slot boundaries, plus one a slot earlier so the counter's
// window anchor is unambiguous. The steady-watts simulator is more realistic but its
// sampling leaves the first and last slots a little short, which is invisible when
// comparing two intervals and fatal to an absolute assertion that assumes every slot
// holds the same energy.
func exactSlotSim(id string, start time.Time, slots int, kwhPerSlot float64) *influx.CounterSim {
	at := make([]time.Time, 0, slots+2)
	kwh := make([]float64, 0, slots+2)
	at = append(at, start.Add(-30*time.Minute))
	kwh = append(kwh, 0)
	for i := 0; i <= slots; i++ {
		at = append(at, start.Add(time.Duration(i)*30*time.Minute))
		kwh = append(kwh, float64(i)*kwhPerSlot)
	}
	return influx.NewCounterSim(time.UTC).AddSamples(id, at, kwh)
}

// ciBuildExact runs BuildSeries over `slots` half hours from start with exact
// per-slot energy, in loc, at the given display interval.
func ciBuildExact(t *testing.T, ivToken string, pricer Pricer, start time.Time, slots int, kwhPerSlot float64, loc *time.Location) Series {
	t.Helper()
	iv, ok := lookupInterval(ivToken)
	if !ok {
		t.Fatalf("unknown interval %q", ivToken)
	}
	win := Window{Label: "custom", Start: start, Stop: start.Add(time.Duration(slots) * 30 * time.Minute)}
	sim := exactSlotSim("winefridge", start, slots, kwhPerSlot)
	devices := map[string]config.DeviceConfig{"winefridge": {Class: "continuous_power_device"}}

	resp, err := BuildSeries(context.Background(), &influx.FakeQuerier{QueryFunc: sim.Answer},
		"b", win, iv, GroupByDevice, false, false, devices, pricer, nil, loc)
	if err != nil {
		t.Fatalf("BuildSeries at %s: %v", ivToken, err)
	}
	if len(resp.Series) != 1 {
		t.Fatalf("series = %d, want 1", len(resp.Series))
	}
	return resp.Series[0]
}

// ciExpectedCost runs the 30-MINUTE axis and computes what the window must cost from
// that axis's per-bucket energy and the given rate table.
//
// The reference energy comes from the 30m run rather than from the fixture's nominal
// figure because `increase()` over a half-open window cannot see energy accruing after
// the last reading inside it — so the simulator delivers one slot less than a naive
// sum expects. That is a property of counter semantics, not of pricing, and pinning it
// here would be testing the fake.
//
// What matters is that this is still INDEPENDENT of the code under test: the coarse
// axes are what is being checked, and the 30m axis is separately established as
// correct (it is the grid the prices are defined on, and TestSeriesTotalCostAgreesWithCostBuckets
// ties it to CostBuckets). Multiplying its per-slot energy by the fixture's per-slot
// rates is arithmetic this file does itself.
func ciExpectedCost(t *testing.T, pricer Pricer, start time.Time, slots int, perSlot float64, loc *time.Location, pence []float64) (float64, Series) {
	t.Helper()
	ref := ciBuildExact(t, "30m", pricer, start, slots, perSlot, loc)
	if len(ref.KWh) != slots {
		t.Fatalf("the 30m reference has %d buckets, want %d", len(ref.KWh), slots)
	}
	var want float64
	for i := 0; i < slots && i < len(pence); i++ {
		want += ref.KWh[i] * pence[i] / 100
	}
	return want, ref
}

// ciWindow is the whole of ciDay.
func ciWindow() Window {
	return Window{Label: "custom", Start: ciDay, Stop: ciDay.Add(24 * time.Hour)}
}

// ciBuild runs BuildSeries over ciDay at the given display interval, against a
// steady-load counter simulator, and returns the one device series.
func ciBuild(t *testing.T, ivToken string, pricer Pricer, watts float64) Series {
	t.Helper()
	iv, ok := lookupInterval(ivToken)
	if !ok {
		t.Fatalf("unknown interval %q", ivToken)
	}
	win := ciWindow()
	// Sample every 5 minutes so any bucketing the code picks is well covered.
	sim := influx.NewCounterSim(time.UTC).
		AddSteady("winefridge", win.Start.Add(-time.Hour), win.Stop.Add(time.Hour), 5*time.Minute, watts)
	devices := map[string]config.DeviceConfig{"winefridge": {Class: "continuous_power_device"}}

	resp, err := BuildSeries(context.Background(), &influx.FakeQuerier{QueryFunc: sim.Answer},
		"b", win, iv, GroupByDevice, false, false, devices, pricer, nil, time.UTC)
	if err != nil {
		t.Fatalf("BuildSeries at %s: %v", ivToken, err)
	}
	if len(resp.Series) != 1 {
		t.Fatalf("series = %d, want 1", len(resp.Series))
	}
	// The response must still describe the axis the CALLER asked for.
	if resp.Interval != ivToken {
		t.Errorf("resp.Interval = %q, want the requested %q", resp.Interval, ivToken)
	}
	return resp.Series[0]
}

// ---------------------------------------------------------------------------

// costAxisFor is the decision itself: cost on the finer of the two, and leave a flat
// tariff alone so a monthly chart does not start issuing 1488-bucket queries to reach
// the answer it already had.
func TestCostAxisFor(t *testing.T) {
	slot, _ := ciPricer()
	flat := FlatPricer{RatePerKWh: 0.25, known: true}

	tests := []struct {
		name    string
		display string
		pricer  Pricer
		want    string
	}{
		{"flat tariff keeps a daily axis", "1d", flat, "1d"},
		{"flat tariff keeps an hourly axis", "1h", flat, "1h"},
		{"slot prices force 30m on a daily axis", "1d", slot, "30m"},
		{"slot prices force 30m on a 6h axis", "6h", slot, "30m"},
		{"slot prices force 30m on an hourly axis", "1h", slot, "30m"},
		{"an axis already at the grid is untouched", "30m", slot, "30m"},
		// Finer than the grid needs no help: the rate is constant within the bucket.
		{"an axis finer than the grid is untouched", "5m", slot, "5m"},
		// A pricer that declares nothing is treated as constant, which is what keeps
		// the expensive path opt-in.
		{"a pricer with no granularity is treated as flat", "1d", constPricer{}, "1d"},
		// An unpriced half-hourly stretch still declares the grid, so its energy is
		// attributed to unpriced_kwh at slot resolution rather than by one instant.
		{"unpriced slots still force the grid", "1d", UnpricedSlots{}, "30m"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			display, ok := lookupInterval(tc.display)
			if !ok {
				t.Fatalf("unknown interval %q", tc.display)
			}
			if got := costAxisFor(display, tc.pricer); got.Token != tc.want {
				t.Errorf("costAxisFor(%s) = %s, want %s", tc.display, got.Token, tc.want)
			}
		})
	}
}

// constPricer implements Pricer but NOT Granularity, standing in for any pricer that
// declines to say — which must be treated as constant rather than assumed fine.
type constPricer struct{}

func (constPricer) RateAt(time.Time) (float64, bool) { return 0.25, true }

// ---------------------------------------------------------------------------

// The headline assertion: the same energy over the same day costs the same whatever
// display interval was requested. Cost is a property of when the energy was used, not
// of how coarsely a chart draws it.
func TestSeriesCostIsTheSameAtEveryDisplayInterval(t *testing.T) {
	pricer, pence := ciPricer()
	const perSlot = 0.5 // kWh in every half hour

	truth, ref := ciExpectedCost(t, pricer, ciDay, len(pence), perSlot, time.UTC, pence)

	for _, ivToken := range []string{"30m", "1h", "6h", "1d"} {
		t.Run(ivToken, func(t *testing.T) {
			s := ciBuildExact(t, ivToken, pricer, ciDay, len(pence), perSlot, time.UTC)

			// Energy must be identical whatever the axis — this fix is about the money.
			if math.Abs(s.TotalKWh-ref.TotalKWh) > 1e-6 {
				t.Errorf("TotalKWh at %s = %v, want %v", ivToken, s.TotalKWh, ref.TotalKWh)
			}
			if math.Abs(s.TotalCost-truth) > 1e-3 {
				t.Errorf("TotalCost at %s = %.4f, want %.4f (%+.1f%%) — a %s bucket priced at "+
					"its FIRST half hour's rate. The same energy over the same day cannot cost "+
					"a different amount because a chart asked for coarser buckets.",
					ivToken, s.TotalCost, truth, 100*(s.TotalCost-truth)/truth, ivToken)
			}
		})
	}
}

// The specific wrong answer, named: a single 1d bucket priced at the 00:00 slot
// charges the whole day at the cheapest overnight rate.
func TestADailyBucketIsNotPricedAtMidnightsRate(t *testing.T) {
	pricer, pence := ciPricer()
	s := ciBuild(t, "1d", pricer, 1000)

	atMidnightRate := s.TotalKWh * pence[0] / 100
	if math.Abs(s.TotalCost-atMidnightRate) < 1e-3 {
		t.Errorf("the whole day was charged at the 00:00 slot's %.0fp: £%.4f for %v kWh",
			pence[0], s.TotalCost, s.TotalKWh)
	}
	// It must be dearer than that, because most of the day is dearer than midnight.
	if s.TotalCost <= atMidnightRate {
		t.Errorf("cost £%.4f is not above the all-at-midnight figure £%.4f",
			s.TotalCost, atMidnightRate)
	}
}

// Measured on the branch's own recorded mixed-sign day — a real published curve, and
// the exact comparison the review quoted.
func TestSeriesCostAtEveryIntervalOnTheRealRecordedDay(t *testing.T) {
	raw, starts, _ := realDay(t)
	if len(starts) != 48 {
		t.Fatalf("fixture has %d slots, want 48", len(starts))
	}
	// Re-stamp the recorded curve onto the test day so it lines up with the window.
	pricer := shiftPricer{inner: raw, by: ciDay.Sub(starts[0])}

	ref := ciBuild(t, "30m", pricer, 1000)
	for _, ivToken := range []string{"1h", "6h", "1d"} {
		t.Run(ivToken, func(t *testing.T) {
			s := ciBuild(t, ivToken, pricer, 1000)
			t.Logf("%s: £%.4f (30m reference £%.4f, %+.2f%%)", ivToken, s.TotalCost,
				ref.TotalCost, 100*(s.TotalCost-ref.TotalCost)/ref.TotalCost)
			if math.Abs(s.TotalCost-ref.TotalCost) > 2e-3 {
				t.Errorf("TotalCost = %.4f, want %.4f", s.TotalCost, ref.TotalCost)
			}
		})
	}
}

// shiftPricer moves a recorded curve in time so a fixture from one day can price
// another. Kept in the test because shifting prices is never right in production.
type shiftPricer struct {
	inner Pricer
	by    time.Duration
}

func (s shiftPricer) RateAt(t time.Time) (float64, bool) { return s.inner.RateAt(t.Add(-s.by)) }
func (s shiftPricer) RateInterval() time.Duration        { return 30 * time.Minute }

// ---------------------------------------------------------------------------

// Folding must not disturb the DISPLAY resolution: a caller asking for 1h buckets
// still gets 24 of them, and the per-bucket costs still sum to the total.
func TestFoldingKeepsTheRequestedResolutionAndSums(t *testing.T) {
	pricer, _ := ciPricer()

	for _, tc := range []struct {
		iv      string
		buckets int
	}{{"1h", 24}, {"6h", 4}, {"1d", 1}} {
		t.Run(tc.iv, func(t *testing.T) {
			iv, _ := lookupInterval(tc.iv)
			win := ciWindow()
			sim := influx.NewCounterSim(time.UTC).
				AddSteady("winefridge", win.Start.Add(-time.Hour), win.Stop.Add(time.Hour), 5*time.Minute, 1000)
			devices := map[string]config.DeviceConfig{"winefridge": {Class: "continuous_power_device"}}
			resp, err := BuildSeries(context.Background(), &influx.FakeQuerier{QueryFunc: sim.Answer},
				"b", win, iv, GroupByDevice, false, false, devices, pricer, nil, time.UTC)
			if err != nil {
				t.Fatal(err)
			}
			if len(resp.Buckets) != tc.buckets {
				t.Errorf("buckets = %d, want the %d the caller asked for", len(resp.Buckets), tc.buckets)
			}
			s := resp.Series[0]
			if len(s.KWh) != tc.buckets || len(s.Cost) != tc.buckets || len(s.AvgW) != tc.buckets {
				t.Fatalf("arrays %d/%d/%d, want %d each", len(s.KWh), len(s.Cost), len(s.AvgW), tc.buckets)
			}
			var sumCost, sumKWh float64
			for i := range s.Cost {
				sumCost += s.Cost[i]
				sumKWh += s.KWh[i]
			}
			if math.Abs(sumCost-s.TotalCost) > 1e-3 {
				t.Errorf("per-bucket costs sum to %v but TotalCost is %v", sumCost, s.TotalCost)
			}
			if math.Abs(sumKWh-s.TotalKWh) > 1e-3 {
				t.Errorf("per-bucket kWh sum to %v but TotalKWh is %v", sumKWh, s.TotalKWh)
			}
			// AvgW is not asserted here: CounterSim answers energy_kwh only, so the
			// power series is legitimately empty in these tests. The hours-weighted
			// mean is covered directly in TestFoldSeriesAveragesWatts.
			if len(s.AvgW) != tc.buckets {
				t.Errorf("AvgW length = %d, want %d", len(s.AvgW), tc.buckets)
			}
		})
	}
}

// A bursty load is the case an approximation would get wrong. Spreading a coarse
// bucket's energy evenly across its slots is exact for a fridge and badly wrong for a
// dishwasher — which is precisely the load this tariff exists to shift, so it is the
// one that must be right.
func TestBurstyLoadIsCostedWhereItActuallyRan(t *testing.T) {
	pricer, pence := ciPricer()
	win := ciWindow()
	devices := map[string]config.DeviceConfig{"dishwasher": {Class: "continuous_power_device"}}

	// 2 kWh consumed entirely inside 02:00–03:00, the cheap overnight stretch.
	at := []time.Time{
		win.Start,
		win.Start.Add(2 * time.Hour),
		win.Start.Add(3 * time.Hour),
		win.Stop,
	}
	kwh := []float64{0, 0, 2, 2}
	sim := influx.NewCounterSim(time.UTC).AddSamples("dishwasher", at, kwh)

	iv, _ := lookupInterval("1d")
	resp, err := BuildSeries(context.Background(), &influx.FakeQuerier{QueryFunc: sim.Answer},
		"b", win, iv, GroupByDevice, false, false, devices, pricer, nil, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	s := resp.Series[0]

	// It ran in the 5p stretch, so it costs the cheap rate — not the day's mean.
	wantCheap := s.TotalKWh * pence[0] / 100
	var mean float64
	for _, p := range pence {
		mean += p
	}
	mean /= float64(len(pence))
	atMean := s.TotalKWh * mean / 100

	t.Logf("%v kWh run overnight: £%.4f (at the overnight rate £%.4f, at the day mean £%.4f)",
		s.TotalKWh, s.TotalCost, wantCheap, atMean)
	if math.Abs(s.TotalCost-wantCheap) > 5e-3 {
		t.Errorf("TotalCost = %.4f, want %.4f — energy must be priced where it actually "+
			"ran, not spread across the bucket it is displayed in", s.TotalCost, wantCheap)
	}
	if math.Abs(s.TotalCost-atMean) < 5e-3 {
		t.Errorf("TotalCost equals the day-mean figure £%.4f; the load's timing was lost", atMean)
	}
}

// A day only PARTLY covered by the archive must report the uncovered energy at slot
// resolution rather than judging a whole coarse bucket by its first instant. A bucket
// priced at its start would call this day wholly priced.
func TestCoarseBucketReportsUnpricedAtSlotResolution(t *testing.T) {
	// Only the first 24 slots (midnight to noon) have a price; pence is 0 beyond, so
	// the expected cost covers the priced half alone.
	pence := make([]float64, 48)
	rates := make([]*float64, 48)
	for i := 0; i < 24; i++ {
		v := 20.0
		pence[i] = v
		rates[i] = &v
	}
	pricer := slotRates{start: ciDay, rates: rates}

	const perSlot = 0.5
	truth, ref := ciExpectedCost(t, pricer, ciDay, 48, perSlot, time.UTC, pence)

	// The unpriced energy is the reference axis's own kWh in the unpriced slots.
	var wantUnpriced float64
	for i := 24; i < 48; i++ {
		wantUnpriced += ref.KWh[i]
	}

	s := ciBuildExact(t, "1d", pricer, ciDay, 48, perSlot, time.UTC)

	// A bucket judged by its start instant would report NONE of this, because
	// midnight is priced.
	if math.Abs(s.UnpricedKWh-wantUnpriced) > 1e-3 {
		t.Errorf("UnpricedKWh = %v, want %v — half the day had no price, and a bucket "+
			"priced only at its start instant cannot see that", s.UnpricedKWh, wantUnpriced)
	}
	if wantUnpriced == 0 {
		t.Fatal("the fixture reported no unpriced energy; it cannot test this")
	}
	if math.Abs(s.TotalCost-truth) > 1e-3 {
		t.Errorf("TotalCost = %v, want %v (the priced half only)", s.TotalCost, truth)
	}
}

// Folding maps fine buckets to display buckets by TIMESTAMP, not by a fixed ratio,
// because a local calendar day is 46, 48 or 50 half hours across a DST changeover. A
// ratio would misalign every bucket after the transition — on the one day of the year
// nobody checks.
func TestFoldingAcrossADSTBoundary(t *testing.T) {
	loc := mustLondon(t)

	for _, tc := range []struct {
		name  string
		start time.Time
		slots int
	}{
		// BST begins: the local day is 23 hours, so 46 half hours.
		{"spring forward (46 half hours)", time.Date(2027, 3, 28, 0, 0, 0, 0, loc), 46},
		// BST ends: 25 hours, 50 half hours.
		{"fall back (50 half hours)", time.Date(2027, 10, 31, 0, 0, 0, 0, loc), 50},
	} {
		t.Run(tc.name, func(t *testing.T) {
			start := tc.start
			stop := start.AddDate(0, 0, 1)
			if got := int(stop.Sub(start) / (30 * time.Minute)); got != tc.slots {
				t.Fatalf("the fixture day is %d half hours, want %d", got, tc.slots)
			}

			// A flat 20p curve across the whole real day: the assertion is about the
			// AXIS arithmetic, and a varying curve would confound the two.
			rates := make([]*float64, tc.slots)
			for i := range rates {
				v := 20.0
				rates[i] = &v
			}
			pricer := slotRates{start: start.UTC(), rates: rates}

			const perSlot = 0.5
			pence := make([]float64, tc.slots)
			for i := range pence {
				pence[i] = 20.0
			}
			truth, ref := ciExpectedCost(t, pricer, start, tc.slots, perSlot, loc, pence)

			s := ciBuildExact(t, "1d", pricer, start, tc.slots, perSlot, loc)

			// The day's REAL length, not an assumed 48 half hours. A fold that mapped
			// fine buckets to display buckets by a fixed RATIO would misalign every
			// bucket after the transition.
			t.Logf("%s: %d slots, %v kWh, £%.4f", tc.name, tc.slots, s.TotalKWh, s.TotalCost)
			if math.Abs(s.TotalKWh-ref.TotalKWh) > 1e-6 {
				t.Errorf("TotalKWh = %v, want %v (%d half hours)", s.TotalKWh, ref.TotalKWh, tc.slots)
			}
			if math.Abs(s.TotalCost-truth) > 1e-3 {
				t.Errorf("TotalCost = %v, want %v", s.TotalCost, truth)
			}
			if s.UnpricedKWh != 0 {
				t.Errorf("UnpricedKWh = %v, want 0 — the curve covers the whole day", s.UnpricedKWh)
			}
		})
	}
}

// A flat tariff must NOT pay for the finer query. The cost path only goes to 30m when
// the price actually varies inside a bucket, which is what keeps an all-flat
// deployment's monthly chart at one query per day.
func TestFlatTariffDoesNotQueryAtSlotResolution(t *testing.T) {
	win := ciWindow()
	iv, _ := lookupInterval("1d")
	sim := influx.NewCounterSim(time.UTC).
		AddSteady("winefridge", win.Start.Add(-time.Hour), win.Stop.Add(time.Hour), 5*time.Minute, 1000)
	fake := &influx.FakeQuerier{QueryFunc: sim.Answer}
	devices := map[string]config.DeviceConfig{"winefridge": {Class: "continuous_power_device"}}

	_, err := BuildSeries(context.Background(), fake, "b", win, iv, GroupByDevice,
		false, false, devices, FlatPricer{RatePerKWh: 0.25, known: true}, nil, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range fake.Queries {
		if strings.Contains(q, "30m") {
			t.Errorf("a flat tariff queried at 30m; the finer axis buys nothing when the "+
				"rate cannot change inside a bucket:\n%s", q)
		}
	}
}

// ---------------------------------------------------------------------------
// foldSeries directly, for the parts the counter simulator cannot reach.
// ---------------------------------------------------------------------------

// Average watts must be an HOURS-WEIGHTED mean when folded, not a sum. Summing gives
// a "wattage" of 48 kW for a 1 kW load over a day, which is not a wattage at all.
func TestFoldSeriesAveragesWatts(t *testing.T) {
	start := ciDay
	fine := make([]time.Time, 48)
	hrs := make([]float64, 48)
	for i := range fine {
		fine[i] = start.Add(time.Duration(i) * 30 * time.Minute)
		hrs[i] = 0.5
	}
	display := []time.Time{start}

	s := Series{
		Key:  "d",
		KWh:  make([]float64, 48),
		Cost: make([]float64, 48),
		AvgW: make([]float64, 48),
	}
	for i := range s.AvgW {
		s.AvgW[i] = 1000 // a steady 1 kW all day
	}

	out := foldSeries(s, fine, display, hrs, foldIndex(fine, display))
	if len(out.AvgW) != 1 {
		t.Fatalf("AvgW = %d buckets, want 1", len(out.AvgW))
	}
	if math.Abs(out.AvgW[0]-1000) > 0.1 {
		t.Errorf("AvgW = %v, want 1000 — a steady 1 kW load is 1 kW however wide the "+
			"display bucket; %v suggests the watts were summed", out.AvgW[0], out.AvgW[0])
	}
}

// Buckets of UNEQUAL length must weight by hours, which is what a DST day produces:
// the same watts held for 1 h and for 0.5 h do not average to the midpoint.
func TestFoldSeriesWeightsUnequalBuckets(t *testing.T) {
	start := ciDay
	fine := []time.Time{start, start.Add(time.Hour)}
	hrs := []float64{1.0, 0.5} // the second bucket is half as long
	display := []time.Time{start}

	s := Series{
		Key:  "d",
		KWh:  []float64{1, 1},
		Cost: []float64{0.2, 0.2},
		AvgW: []float64{1000, 400},
	}
	out := foldSeries(s, fine, display, hrs, foldIndex(fine, display))

	// (1000×1 + 400×0.5) / 1.5 = 800, not the unweighted mean of 700.
	const want = 800.0
	if math.Abs(out.AvgW[0]-want) > 0.1 {
		t.Errorf("AvgW = %v, want %v (hours-weighted); the unweighted mean would be 700",
			out.AvgW[0], want)
	}
}

// Folding sums the FULL-PRECISION carriers, not the rounded wire values. Summing 48
// costs already rounded to sub-penny reintroduces, per display bucket, the drift this
// package fixes at the window level.
func TestFoldSeriesSumsFullPrecisionNotRoundedValues(t *testing.T) {
	const n = 48
	start := ciDay
	fine := make([]time.Time, n)
	hrs := make([]float64, n)
	for i := range fine {
		fine[i] = start.Add(time.Duration(i) * 30 * time.Minute)
		hrs[i] = 0.5
	}
	display := []time.Time{start}

	// Each bucket's true cost is £0.000149, which rounds DOWN to £0.0001 on the wire.
	raw := make([]float64, n)
	wire := make([]float64, n)
	for i := range raw {
		raw[i] = 0.000149
		wire[i] = round.To(raw[i], round.MoneyDP)
	}
	s := Series{
		Key: "d", KWh: make([]float64, n), Cost: wire, AvgW: make([]float64, n),
		rawKWh: make([]float64, n), rawCost: raw,
	}

	out := foldSeries(s, fine, display, hrs, foldIndex(fine, display))

	want := round.To(0.000149*n, round.MoneyDP)   // 0.0072
	fromWire := round.To(0.0001*n, round.MoneyDP) // 0.0048, a 33% understatement
	if math.Abs(out.Cost[0]-want) > 1e-9 {
		t.Errorf("folded Cost = %v, want %v", out.Cost[0], want)
	}
	if math.Abs(out.Cost[0]-fromWire) < 1e-9 {
		t.Errorf("folded Cost = %v, which is the sum of the ROUNDED per-bucket values; "+
			"folding must use the full-precision carriers", out.Cost[0])
	}
}

// A series with no carriers (one that was never built on a fine axis) must still fold
// sanely from its wire values rather than producing zeros — the fallback path.
func TestFoldSeriesWithoutCarriers(t *testing.T) {
	start := ciDay
	fine := []time.Time{start, start.Add(30 * time.Minute)}
	hrs := []float64{0.5, 0.5}
	display := []time.Time{start}

	s := Series{Key: "d", KWh: []float64{1, 2}, Cost: []float64{0.2, 0.4}, AvgW: []float64{0, 0}}
	out := foldSeries(s, fine, display, hrs, foldIndex(fine, display))

	if out.KWh[0] != 3 {
		t.Errorf("KWh = %v, want 3", out.KWh[0])
	}
	if math.Abs(out.Cost[0]-0.6) > 1e-9 {
		t.Errorf("Cost = %v, want 0.6", out.Cost[0])
	}
}

// foldIndex maps by timestamp, so a fine bucket landing exactly on a display boundary
// belongs to the LATER display bucket — the same half-open rule the rest of the
// service uses. Getting it backwards shifts a day's worth of cost by one bucket.
func TestFoldIndexIsHalfOpen(t *testing.T) {
	start := ciDay
	fine := []time.Time{
		start,
		start.Add(30 * time.Minute),
		start.Add(time.Hour), // exactly the second display boundary
		start.Add(90 * time.Minute),
	}
	display := []time.Time{start, start.Add(time.Hour)}

	got := foldIndex(fine, display)
	want := []int{0, 0, 1, 1}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("foldIndex[%d] = %d, want %d — a fine bucket on a display boundary "+
				"belongs to the LATER bucket", i, got[i], want[i])
		}
	}
}
