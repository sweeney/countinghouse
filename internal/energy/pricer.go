package energy

import (
	"sort"
	"time"

	"github.com/sweeney/countinghouse/internal/config"
)

// Pricer answers what a kWh costs at one instant.
//
// It replaces the `kWh × tariff.UnitRate × VAT` multiplication that every cost in
// this service used to be. That multiplication has no way to express "the rate
// varies within the window" and no way to express "I do not know" — so under a
// half-hourly tariff, where UnitRate is deliberately zero because the rate lives
// in the price archive, it returned a confident £0.00 for real consumption.
//
// The returned rate is **VAT-INCLUSIVE £ per kWh**. The gross-up happens inside
// the pricer rather than at each call site, because a VAT rate is a property of
// the tariff in force at that instant and so belongs with the rate itself — which
// is also what makes a window spanning a VAT change price correctly.
type Pricer interface {
	// RateAt returns the VAT-inclusive £/kWh applying at t.
	//
	// The bool is load-bearing: false means NO PRICE IS KNOWN, which a caller must
	// surface as unpriced energy rather than treat as free. Returning (0, true)
	// would be a lie, and it is the specific lie this interface exists to prevent.
	RateAt(t time.Time) (float64, bool)
}

// FlatPricer prices every instant at one rate.
type FlatPricer struct {
	// RatePerKWh is VAT-inclusive £/kWh. Zero means UNPRICED rather than free —
	// see FlatPricerFor.
	RatePerKWh float64

	known bool
}

// FlatPricerFor builds a flat pricer from a resolved tariff.
//
// A HALF-HOURLY tariff yields a pricer that knows nothing, deliberately. Its
// UnitRate is zero because the rate lives in the archive, so pricing from it would
// charge nothing for real energy — exactly the silent-zero this package is being
// changed to prevent. Better to report every slot unpriced and be obviously
// broken than to report a plausible £0.00.
func FlatPricerFor(t config.Tariff) FlatPricer {
	if t.IsHalfHourly() {
		return FlatPricer{}
	}
	return FlatPricer{RatePerKWh: t.UnitRate * t.Multiplier(), known: true}
}

// RateAt implements Pricer.
func (f FlatPricer) RateAt(time.Time) (float64, bool) {
	if !f.known {
		return 0, false
	}
	return f.RatePerKWh, true
}

// PricedSegment is one sub-range of a window with its own pricer.
// Start is inclusive, Stop exclusive.
type PricedSegment struct {
	Start, Stop time.Time
	Pricer      Pricer
}

// SegmentedPricer dispatches across tariff periods.
//
// Not a corner case: the first bill after a switchover necessarily spans one, so
// this is the shape of the very next bill. Each side prices on its own tariff with
// its own VAT multiplier, and an instant covered by no segment is UNKNOWN rather
// than borrowed from a neighbour.
type SegmentedPricer struct {
	Segments []PricedSegment
}

// RateAt implements Pricer, finding the segment covering t.
func (s SegmentedPricer) RateAt(t time.Time) (float64, bool) {
	for _, seg := range s.sorted() {
		// Half-open, so the boundary instant belongs to the LATER segment. Getting
		// this backwards bills one half hour on the wrong tariff at every
		// switchover — small, and permanently irreproducible.
		if !t.Before(seg.Start) && t.Before(seg.Stop) {
			if seg.Pricer == nil {
				return 0, false
			}
			return seg.Pricer.RateAt(t)
		}
	}
	return 0, false
}

func (s SegmentedPricer) sorted() []PricedSegment {
	out := make([]PricedSegment, len(s.Segments))
	copy(out, s.Segments)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Start.Before(out[j].Start) })
	return out
}

// CostBuckets prices per-bucket energy at each bucket's own rate.
//
// This is the function the silent-zero bug lived in. Returns the VAT-inclusive £
// cost of the energy it could price, and separately the kWh it could NOT — which
// the caller must surface rather than swallow. Charging nothing for real
// consumption is the quiet way a bill comes out wrong.
//
// An empty bucket with no known price is NOT reported as unpriced: there is no
// energy needing a price, and counting it would turn every overnight hour of an
// idle device into noise in a field that needs to stay meaningful.
func CostBuckets(buckets []time.Time, kwh []float64, p Pricer) (cost, unpricedKWh float64) {
	for i, start := range buckets {
		if i >= len(kwh) {
			break
		}
		energy := kwh[i]
		if energy == 0 {
			continue
		}
		rate, known := p.RateAt(start)
		if !known {
			unpricedKWh += energy
			continue
		}
		cost += energy * rate
	}
	return cost, unpricedKWh
}

// EffectiveRate returns cost ÷ kWh, the VAT-inclusive £/kWh actually paid.
//
// Reported per device because it is what makes a slot-priced figure interpretable
// — the whole reason the counter-quantisation noise in per-device costs is
// acceptable (see docs/per-device-attribution.md). Zero energy has no effective
// rate, and returns zero rather than NaN or Inf, neither of which survives JSON.
func EffectiveRate(cost, kwh float64) float64 {
	if kwh == 0 {
		return 0
	}
	return cost / kwh
}

// CostingInterval is the bucketing the cost path uses when prices are half-hourly.
//
// It deliberately BYPASSES MaxBuckets. That cap is a response-size guard for
// /series — a month at 30m is 1488 buckets, which would blow up a chart payload —
// but the cost path reduces those buckets to a handful of scalars, so the cap
// protects nothing here and would instead make a month's bill unanswerable.
func CostingInterval() Interval {
	iv, ok := lookupInterval("30m")
	if !ok {
		// Unreachable: "30m" is in the allowed set. Guarded rather than panicking
		// because a bill refusing is better than a process dying.
		return Interval{Token: "30m", Duration: 30 * time.Minute}
	}
	return iv
}
