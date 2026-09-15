package energy

import (
	"math"
	"testing"
	"time"

	"github.com/sweeney/countinghouse/internal/config"
)

// ---------------------------------------------------------------------------
// Pricer: how a kWh becomes money.
//
// Until now every cost in this service was `kWh × tariff.UnitRate × VAT`. That
// breaks completely under a half-hourly tariff, where UnitRate is deliberately
// ZERO because the rate lives in the price archive — so every cost on /series,
// /bill and /devices/{id}/cost would have been reported as £0.00 the moment the
// agreements namespace was switched on. Confident zeros, in the shape of a
// correct answer.
//
// Pricer replaces that multiplication. It answers one question — what does a kWh
// cost at THIS instant — and it can answer "I do not know", which is the part a
// bare multiplication could never express.
// ---------------------------------------------------------------------------

func mustTS(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("bad timestamp %q: %v", s, err)
	}
	return ts.UTC()
}

// ---------------------------------------------------------------------------
// Flat
// ---------------------------------------------------------------------------

// A flat tariff prices every instant the same, and the VAT gross-up happens once
// inside the pricer rather than at each call site — which is where it used to be
// forgotten.
func TestFlatPricer(t *testing.T) {
	tariff := config.Tariff{UnitRate: 0.208948, VATRate: 0.05}
	p := FlatPricerFor(tariff)

	want := 0.208948 * 1.05
	for _, when := range []string{
		"2020-01-01T00:00:00Z", "2026-09-12T13:37:00Z", "2030-12-31T23:59:59Z",
	} {
		got, ok := p.RateAt(mustTS(t, when))
		if !ok {
			t.Fatalf("a flat tariff must price every instant; %s was unknown", when)
		}
		if math.Abs(got-want) > 1e-12 {
			t.Errorf("%s: rate = %v, want %v (VAT-inclusive £/kWh)", when, got, want)
		}
	}
}

// A half-hourly tariff has no UnitRate, so building a flat pricer from one must
// refuse rather than silently price everything at zero. This is the exact bug
// Pricer exists to prevent, so it is pinned at the constructor.
func TestFlatPricerRefusesAHalfHourlyTariff(t *testing.T) {
	half := config.Tariff{TariffCode: "E-1R-AGILE-24-10-01-A", VATRate: 0.05}
	p := FlatPricerFor(half)

	if _, ok := p.RateAt(mustTS(t, "2026-09-12T12:00:00Z")); ok {
		t.Error("a flat pricer built from a half-hourly tariff must report UNKNOWN, " +
			"never zero — a zero rate is a confident wrong answer")
	}
}

// ---------------------------------------------------------------------------
// Slot-backed
// ---------------------------------------------------------------------------

// slotRates is a minimal Pricer over explicit half-hourly rates, standing in for
// the price archive so this package need not depend on it.
type slotRates struct {
	start time.Time
	// incVATPencePerKWh, one per half hour from start. nil leaves a GAP.
	rates []*float64
}

// RateInterval declares the half-hour grid, satisfying Granularity — so a test
// using this fixture exercises the same cost-axis choice production does.
func (s slotRates) RateInterval() time.Duration { return 30 * time.Minute }

func (s slotRates) RateAt(t time.Time) (float64, bool) {
	if t.Before(s.start) {
		return 0, false
	}
	i := int(t.Sub(s.start) / (30 * time.Minute))
	if i < 0 || i >= len(s.rates) || s.rates[i] == nil {
		return 0, false
	}
	return *s.rates[i] / 100, true // pence → £
}

func fp(v float64) *float64 { return &v }

func TestSlotPricerResolvesPerHalfHour(t *testing.T) {
	start := mustTS(t, "2026-09-12T00:00:00Z")
	p := slotRates{start: start, rates: []*float64{fp(20), fp(40), nil, fp(60)}}

	for _, tc := range []struct {
		name string
		at   string
		want float64
		ok   bool
	}{
		{name: "the first slot's own start", at: "2026-09-12T00:00:00Z", want: 0.20, ok: true},
		{name: "inside the first slot", at: "2026-09-12T00:29:59Z", want: 0.20, ok: true},
		{name: "the boundary belongs to the NEXT slot", at: "2026-09-12T00:30:00Z", want: 0.40, ok: true},
		{name: "a gap is unknown, not free", at: "2026-09-12T01:00:00Z", ok: false},
		{name: "after the last slot is unknown", at: "2026-09-12T02:30:00Z", ok: false},
		{name: "before the first slot is unknown", at: "2026-09-11T23:00:00Z", ok: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := p.RateAt(mustTS(t, tc.at))
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if ok && math.Abs(got-tc.want) > 1e-12 {
				t.Errorf("rate = %v, want %v", got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Segmented — a window spanning a tariff change
// ---------------------------------------------------------------------------

// The first Agile bill necessarily spans the switchover, so this is not a corner
// case: it is the shape of the very next bill. Each side must price on its own
// tariff, with its own VAT multiplier.
func TestSegmentedPricerSwitchesAtTheBoundary(t *testing.T) {
	switchover := mustTS(t, "2026-09-10T00:00:00Z")
	start := switchover

	flat := config.Tariff{UnitRate: 0.20, VATRate: 0.05}
	curve := slotRates{start: start, rates: []*float64{fp(30), fp(50)}}

	p := SegmentedPricer{Segments: []PricedSegment{
		{Start: mustTS(t, "2026-09-01T00:00:00Z"), Stop: switchover, Pricer: FlatPricerFor(flat)},
		{Start: switchover, Stop: mustTS(t, "2026-10-01T00:00:00Z"), Pricer: curve},
	}}

	for _, tc := range []struct {
		name string
		at   string
		want float64
		ok   bool
	}{
		{name: "inside the flat segment", at: "2026-09-05T12:00:00Z", want: 0.20 * 1.05, ok: true},
		{
			// Segment bounds are half-open, so the switchover instant belongs to the
			// NEW tariff. Getting this backwards bills one half hour a year on the
			// wrong tariff — small, and permanently irreproducible.
			name: "exactly at the switchover is the new tariff",
			at:   "2026-09-10T00:00:00Z", want: 0.30, ok: true,
		},
		{name: "the instant before is still the old one", at: "2026-09-09T23:59:59Z", want: 0.20 * 1.05, ok: true},
		{name: "the second slot of the new tariff", at: "2026-09-10T00:30:00Z", want: 0.50, ok: true},
		{
			// Past where the archive reaches: unknown, NOT the flat rate from the
			// previous segment and NOT zero.
			name: "beyond the curve is unknown", at: "2026-09-10T02:00:00Z", ok: false,
		},
		{name: "before every segment is unknown", at: "2026-08-01T00:00:00Z", ok: false},
		{name: "after every segment is unknown", at: "2027-01-01T00:00:00Z", ok: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := p.RateAt(mustTS(t, tc.at))
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if ok && math.Abs(got-tc.want) > 1e-12 {
				t.Errorf("rate = %v, want %v", got, tc.want)
			}
		})
	}
}

// Per-segment VAT, because VAT rates change and a historical bill must use the one
// that applied on the day.
func TestSegmentedPricerAppliesPerSegmentVAT(t *testing.T) {
	boundary := mustTS(t, "2026-04-01T00:00:00Z")
	p := SegmentedPricer{Segments: []PricedSegment{
		{Start: mustTS(t, "2026-01-01T00:00:00Z"), Stop: boundary,
			Pricer: FlatPricerFor(config.Tariff{UnitRate: 0.20, VATRate: 0.05})},
		{Start: boundary, Stop: mustTS(t, "2026-07-01T00:00:00Z"),
			Pricer: FlatPricerFor(config.Tariff{UnitRate: 0.20, VATRate: 0.20})},
	}}

	before, _ := p.RateAt(mustTS(t, "2026-02-01T00:00:00Z"))
	after, _ := p.RateAt(mustTS(t, "2026-05-01T00:00:00Z"))
	if math.Abs(before-0.20*1.05) > 1e-12 {
		t.Errorf("before = %v, want the 5%% rate", before)
	}
	if math.Abs(after-0.20*1.20) > 1e-12 {
		t.Errorf("after = %v, want the 20%% rate", after)
	}
}

func TestSegmentedPricerWithNoSegments(t *testing.T) {
	if _, ok := (SegmentedPricer{}).RateAt(mustTS(t, "2026-09-12T12:00:00Z")); ok {
		t.Error("no segments can price nothing; want unknown rather than zero")
	}
}

// ---------------------------------------------------------------------------
// Costing buckets
// ---------------------------------------------------------------------------

// CostBuckets is where the silent-zero bug actually lived. It must price each
// bucket at ITS OWN rate, and report energy it could not price rather than
// charging nothing for it.
func TestCostBuckets(t *testing.T) {
	start := mustTS(t, "2026-09-12T00:00:00Z")
	buckets := []time.Time{start, start.Add(30 * time.Minute), start.Add(time.Hour)}
	p := slotRates{start: start, rates: []*float64{fp(20), fp(40), fp(60)}}

	// 1 kWh in each half hour, priced 20p, 40p, 60p.
	cost, unpriced := CostBuckets(buckets, []float64{1, 1, 1}, p)
	want := 0.20 + 0.40 + 0.60
	if math.Abs(cost-want) > 1e-12 {
		t.Errorf("cost = %v, want %v — each bucket at its own rate", cost, want)
	}
	if unpriced != 0 {
		t.Errorf("unpriced = %v, want 0", unpriced)
	}
}

// Energy in a slot with no known price must be REPORTED, not priced at zero.
// Charging nothing for real consumption is the quiet way a bill comes out wrong.
func TestCostBucketsReportsUnpricedEnergy(t *testing.T) {
	start := mustTS(t, "2026-09-12T00:00:00Z")
	buckets := []time.Time{start, start.Add(30 * time.Minute), start.Add(time.Hour)}
	p := slotRates{start: start, rates: []*float64{fp(20), nil, fp(60)}}

	cost, unpriced := CostBuckets(buckets, []float64{1, 2.5, 1}, p)
	if math.Abs(cost-(0.20+0.60)) > 1e-12 {
		t.Errorf("cost = %v, want only the priced buckets", cost)
	}
	if math.Abs(unpriced-2.5) > 1e-12 {
		t.Errorf("unpriced = %v, want 2.5 kWh surfaced rather than charged at nothing", unpriced)
	}
}

// Negative prices must reduce the bill, not be clamped. Being paid to consume is
// the tariff working as designed.
func TestCostBucketsHandlesNegativeRates(t *testing.T) {
	start := mustTS(t, "2026-09-12T00:00:00Z")
	buckets := []time.Time{start, start.Add(30 * time.Minute)}
	p := slotRates{start: start, rates: []*float64{fp(-5), fp(25)}}

	cost, unpriced := CostBuckets(buckets, []float64{2, 1}, p)
	want := 2*(-0.05) + 1*0.25
	if math.Abs(cost-want) > 1e-12 {
		t.Errorf("cost = %v, want %v — a negative rate must reduce the bill", cost, want)
	}
	if unpriced != 0 {
		t.Errorf("unpriced = %v", unpriced)
	}
}

// A bucket with no energy needs no price, so an unpriced EMPTY bucket is not a gap
// worth reporting — otherwise every overnight hour of an idle device would surface
// as unpriced energy and the field would become noise.
func TestCostBucketsIgnoresEmptyUnpricedBuckets(t *testing.T) {
	start := mustTS(t, "2026-09-12T00:00:00Z")
	buckets := []time.Time{start, start.Add(30 * time.Minute)}
	p := slotRates{start: start, rates: []*float64{fp(20), nil}}

	cost, unpriced := CostBuckets(buckets, []float64{1, 0}, p)
	if math.Abs(cost-0.20) > 1e-12 {
		t.Errorf("cost = %v", cost)
	}
	if unpriced != 0 {
		t.Errorf("unpriced = %v, want 0: an empty bucket needs no price", unpriced)
	}
}

func TestCostBucketsMismatchedLengths(t *testing.T) {
	start := mustTS(t, "2026-09-12T00:00:00Z")
	p := slotRates{start: start, rates: []*float64{fp(20)}}
	// Defensive: fewer values than buckets must not panic, and must not invent
	// energy for the buckets it has no value for.
	cost, unpriced := CostBuckets([]time.Time{start, start.Add(30 * time.Minute)}, []float64{1}, p)
	if math.Abs(cost-0.20) > 1e-12 || unpriced != 0 {
		t.Errorf("cost = %v unpriced = %v, want 0.20 and 0", cost, unpriced)
	}
}

// EffectiveRate is what makes a per-device figure interpretable — the whole reason
// C1's slot-level noise is acceptable. Zero energy has no effective rate, and
// dividing by it would yield NaN or Inf on the wire.
func TestEffectiveRate(t *testing.T) {
	if got := EffectiveRate(1.05, 5); math.Abs(got-0.21) > 1e-12 {
		t.Errorf("EffectiveRate(1.05, 5) = %v, want 0.21", got)
	}
	if got := EffectiveRate(0, 0); got != 0 {
		t.Errorf("EffectiveRate with no energy = %v, want 0 rather than NaN", got)
	}
	if got := EffectiveRate(-0.5, 2); math.Abs(got-(-0.25)) > 1e-12 {
		t.Errorf("a negative cost should give a negative effective rate, got %v", got)
	}
}
