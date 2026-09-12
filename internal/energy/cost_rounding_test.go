package energy

import (
	"math"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Totals must accumulate at full precision.
//
// The per-bucket costs a series puts on the wire are rounded to sub-penny, which is
// right: nobody wants 0.0012379999999998 in a chart. Summing those ROUNDED values
// into the window total is a different matter. It was harmless while the totals only
// ever fed a chart, and it stopped being harmless when /bill started reading them:
// a month at half-hourly resolution is 1488 buckets, and 1488 roundings of up to
// 0.005p each can drift the bill by several pence in one direction.
//
// Pence on a bill is the kind of wrong that is technically small and completely
// indefensible, so the totals are accumulated raw and rounded exactly once.
// ---------------------------------------------------------------------------

// A long run of buckets whose true cost rounds the SAME WAY every time — each
// bucket's cost sits just above a rounding boundary, so summing the rounded values
// accumulates the error rather than cancelling it. This is the adversarial shape;
// real prices cancel more, which is exactly why a realistic fixture would not catch
// the bug.
func TestSeriesTotalCostDoesNotAccumulateRoundingError(t *testing.T) {
	const n = 1488 // a month of half hours
	start := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)

	buckets := make([]time.Time, n)
	kwh := make([]float64, n)
	for i := range buckets {
		buckets[i] = start.Add(time.Duration(i) * 30 * time.Minute)
		// 0.000149 kWh at £1/kWh costs £0.000149, which rounds DOWN to £0.0001 at
		// money precision — losing 0.000049 every bucket, every time.
		kwh[i] = 0.000149
	}

	flat := FlatPricer{RatePerKWh: 1.0, known: true}
	s := buildSeries("d", "D", "", "continuous_power_device", buckets,
		[][]float64{kwh}, [][]float64{make([]float64, n)}, flat)

	want := 0.000149 * n // £0.2218 or so
	if math.Abs(s.TotalCost-want) > 5e-5 {
		t.Errorf("TotalCost = %v, want %v — off by %v. The total is summing "+
			"per-bucket costs that were already rounded for the wire; over %d buckets "+
			"that drifts the bill.", s.TotalCost, want, want-s.TotalCost, n)
	}

	// The per-bucket values on the wire are still rounded — that part was correct.
	if s.Cost[0] != 0.0001 {
		t.Errorf("Cost[0] = %v, want 0.0001 (per-bucket values stay rounded for the wire)", s.Cost[0])
	}

	// Same argument for energy: the window total must not be a sum of rounded parts.
	wantKWh := 0.000149 * n
	if math.Abs(s.TotalKWh-wantKWh) > 5e-4 {
		t.Errorf("TotalKWh = %v, want %v", s.TotalKWh, wantKWh)
	}
}

// And the series must agree with CostBuckets, which is the other implementation of
// "price these buckets". Two implementations of that sum is how /series and /bill
// come to disagree, and a consumer comparing a chart against a bill has no way to
// tell which one lied.
func TestSeriesTotalCostAgreesWithCostBuckets(t *testing.T) {
	pricer, starts, _ := realDay(t)

	kwh := make([]float64, len(starts))
	for i := range kwh {
		kwh[i] = 0.0335 // a fridge's steady draw
	}

	s := buildSeries("d", "D", "", "continuous_power_device", starts,
		[][]float64{kwh}, [][]float64{make([]float64, len(starts))}, pricer)
	want, unpriced := CostBuckets(starts, kwh, pricer)

	if unpriced != 0 {
		t.Fatalf("the fixture is a complete day but CostBuckets reported %v unpriced", unpriced)
	}
	if math.Abs(s.TotalCost-want) > 5e-5 {
		t.Errorf("series TotalCost = %v but CostBuckets says %v; the two must agree",
			s.TotalCost, want)
	}
}

// Unpriced energy must likewise total at full precision, and must come from the
// same place the cost does.
func TestSeriesUnpricedTotalsExactly(t *testing.T) {
	const n = 100
	start := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	buckets := make([]time.Time, n)
	kwh := make([]float64, n)
	for i := range buckets {
		buckets[i] = start.Add(time.Duration(i) * 30 * time.Minute)
		kwh[i] = 0.0004 // rounds to 0.000 at kWh precision
	}

	// A pricer that knows nothing: every bucket is unpriced.
	s := buildSeries("d", "D", "", "continuous_power_device", buckets,
		[][]float64{kwh}, [][]float64{make([]float64, n)}, FlatPricer{})

	if s.TotalCost != 0 {
		t.Errorf("TotalCost = %v; nothing could be priced, so nothing may be charged", s.TotalCost)
	}
	want := 0.0004 * n
	if math.Abs(s.UnpricedKWh-want) > 5e-4 {
		t.Errorf("UnpricedKWh = %v, want %v — a total of rounded-away parts is zero, "+
			"which reads as 'nothing was missing'", s.UnpricedKWh, want)
	}
}
