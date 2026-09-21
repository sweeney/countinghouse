package energy

import (
	"math"
	"testing"
	"time"
)

// slotPricer is a half-hourly pricer over an explicit slot table, resolving by
// CONTAINMENT like the real Curve does: the rate for an instant is the rate of
// the slot it falls inside, not of a slot whose start it happens to equal.
type slotPricer struct{ rates map[int64]float64 }

func (s slotPricer) RateAt(t time.Time) (float64, bool) {
	r, ok := s.rates[t.UTC().Truncate(30*time.Minute).Unix()]
	return r, ok
}
func (s slotPricer) RateInterval() time.Duration { return 30 * time.Minute }

// halfHours builds a slotPricer over n consecutive half hours from start.
func halfHours(start time.Time, rates ...float64) slotPricer {
	m := map[int64]float64{}
	for i, r := range rates {
		m[start.UTC().Add(time.Duration(i)*30*time.Minute).Unix()] = r
	}
	return slotPricer{rates: m}
}

// axis builds n bucket starts of width w from start, plus the window stop.
func axis(start time.Time, w time.Duration, n int) ([]time.Time, time.Time) {
	b := make([]time.Time, n)
	for i := range b {
		b[i] = start.Add(time.Duration(i) * w)
	}
	return b, start.Add(time.Duration(n) * w)
}

func mustP(t *testing.T, p *float64) float64 {
	t.Helper()
	if p == nil {
		t.Fatal("price is null, want a value")
	}
	return *p
}

// The contract every consumer relies on, stated as a test: one price per bucket,
// same order, always.
func TestBucketPricesAlignsToTheBucketAxis(t *testing.T) {
	start := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)
	for _, w := range []time.Duration{15 * time.Minute, 30 * time.Minute, time.Hour} {
		buckets, stop := axis(start, w, 7)
		prices, _, _ := BucketPrices(buckets, stop, halfHours(start, 10, 20, 30, 40, 50, 60, 70, 80, 90, 100, 110, 120, 130, 140))
		if len(prices) != len(buckets) {
			t.Errorf("w=%v: len(prices)=%d, len(buckets)=%d", w, len(prices), len(buckets))
		}
	}
}

// A bucket the same width as a slot takes that slot's rate.
func TestBucketPricesAtSlotResolution(t *testing.T) {
	start := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)
	buckets, stop := axis(start, 30*time.Minute, 3)
	prices, basis, unpriced := BucketPrices(buckets, stop, halfHours(start, 0.10, 0.20, 0.30))

	if basis != PriceBasisSlot {
		t.Errorf("basis = %q, want %q", basis, PriceBasisSlot)
	}
	if unpriced != 0 {
		t.Errorf("unpriced = %d, want 0", unpriced)
	}
	for i, want := range []float64{0.10, 0.20, 0.30} {
		if got := mustP(t, prices[i]); math.Abs(got-want) > 1e-9 {
			t.Errorf("prices[%d] = %v, want %v", i, got, want)
		}
	}
}

// Issue #36 N2: at interval=15m the client-side join matched RFC3339 strings and
// silently dropped every odd bucket — half the energy, from the half of the day
// that starts on the hour. A 15m bucket lies INSIDE one half hour, so the server
// can always answer: containment is unambiguous where string equality is not.
func TestBucketPricesAtFinerThanSlotResolution(t *testing.T) {
	start := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)
	buckets, stop := axis(start, 15*time.Minute, 4)
	prices, basis, unpriced := BucketPrices(buckets, stop, halfHours(start, 0.10, 0.20))

	if basis != PriceBasisSlot {
		t.Errorf("basis = %q, want %q", basis, PriceBasisSlot)
	}
	if unpriced != 0 {
		t.Fatalf("unpriced = %d, want 0: every 15m bucket sits inside a slot", unpriced)
	}
	// :00 and :15 are both inside the first half hour; :30 and :45 inside the second.
	for i, want := range []float64{0.10, 0.10, 0.20, 0.20} {
		if got := mustP(t, prices[i]); math.Abs(got-want) > 1e-9 {
			t.Errorf("prices[%d] = %v, want %v", i, got, want)
		}
	}
}

// A bucket spanning several slots reports the TIME-weighted mean. Not
// energy-weighted: that is cost/kwh, undefined in a zero-kWh bucket, and those
// are exactly the buckets that answer "it was cheap and we did NOT use it".
func TestBucketPricesCoarserThanSlotIsTimeWeighted(t *testing.T) {
	start := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)
	buckets, stop := axis(start, time.Hour, 2)
	prices, basis, unpriced := BucketPrices(buckets, stop, halfHours(start, 0.10, 0.30, 0.20, 0.60))

	if basis != PriceBasisMeanOverBucket {
		t.Errorf("basis = %q, want %q", basis, PriceBasisMeanOverBucket)
	}
	if unpriced != 0 {
		t.Fatalf("unpriced = %d, want 0", unpriced)
	}
	for i, want := range []float64{0.20, 0.40} { // (.10+.30)/2, (.20+.60)/2
		if got := mustP(t, prices[i]); math.Abs(got-want) > 1e-9 {
			t.Errorf("prices[%d] = %v, want %v", i, got, want)
		}
	}
}

// A flat tariff has one rate for all time, so there is nothing to average and the
// basis says so.
func TestBucketPricesOnAFlatTariff(t *testing.T) {
	start := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)
	buckets, stop := axis(start, 24*time.Hour, 2)
	prices, basis, unpriced := BucketPrices(buckets, stop, FlatPricer{RatePerKWh: 0.2089, known: true})

	if basis != PriceBasisFlat {
		t.Errorf("basis = %q, want %q", basis, PriceBasisFlat)
	}
	if unpriced != 0 {
		t.Errorf("unpriced = %d, want 0", unpriced)
	}
	for i := range prices {
		if got := mustP(t, prices[i]); math.Abs(got-0.2089) > 1e-9 {
			t.Errorf("prices[%d] = %v, want the flat rate", i, got)
		}
	}
}

// null means NO RATE IS HELD, never free. unpriced_buckets counts them, so 0 is a
// positive assertion that the window is fully priced.
func TestBucketPricesReportsUnknownSlotsAsNull(t *testing.T) {
	start := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)
	buckets, stop := axis(start, 30*time.Minute, 3)
	// Only the first and third half hours are held.
	p := slotPricer{rates: map[int64]float64{
		start.Unix():                       0.10,
		start.Add(60 * time.Minute).Unix(): 0.30,
	}}
	prices, _, unpriced := BucketPrices(buckets, stop, p)

	if prices[1] != nil {
		t.Errorf("prices[1] = %v, want null: no rate is held", *prices[1])
	}
	if unpriced != 1 {
		t.Errorf("unpriced = %d, want 1", unpriced)
	}
}

// A coarse bucket is priced only when EVERY slot inside it is. A mean over the
// slots that happen to be held is a plausible-looking wrong number, which is the
// failure mode this service refuses everywhere else.
func TestCoarseBucketWithAnyMissingSlotIsNull(t *testing.T) {
	start := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)
	buckets, stop := axis(start, time.Hour, 1)
	p := slotPricer{rates: map[int64]float64{start.Unix(): 0.10}} // second half hour missing

	prices, basis, unpriced := BucketPrices(buckets, stop, p)
	if basis != PriceBasisMeanOverBucket {
		t.Errorf("basis = %q, want %q", basis, PriceBasisMeanOverBucket)
	}
	if prices[0] != nil {
		t.Errorf("prices[0] = %v, want null: half the bucket has no rate", *prices[0])
	}
	if unpriced != 1 {
		t.Errorf("unpriced = %d, want 1", unpriced)
	}
}

// A first bucket that starts off the slot grid (from=14:29 with interval=30m
// yields a 14:00 bucket) must still weight by the slots it actually covers.
func TestCoarseBucketOffTheSlotGrid(t *testing.T) {
	start := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)
	// One 45-minute bucket from :15 — 15 min of slot 0, 30 min of slot 1.
	buckets := []time.Time{start.Add(15 * time.Minute)}
	stop := start.Add(60 * time.Minute)
	prices, _, _ := BucketPrices(buckets, stop, halfHours(start, 0.10, 0.40))

	want := (0.10*15 + 0.40*30) / 45
	if got := mustP(t, prices[0]); math.Abs(got-want) > 1e-9 {
		t.Errorf("prices[0] = %v, want %v (time-weighted across the boundary)", got, want)
	}
}

// No pricer at all is not free electricity.
func TestBucketPricesWithNoPricer(t *testing.T) {
	start := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)
	buckets, stop := axis(start, time.Hour, 3)
	prices, _, unpriced := BucketPrices(buckets, stop, nil)
	if len(prices) != 3 || unpriced != 3 {
		t.Fatalf("prices=%d unpriced=%d, want 3 and 3", len(prices), unpriced)
	}
	for i, p := range prices {
		if p != nil {
			t.Errorf("prices[%d] = %v, want null", i, *p)
		}
	}
}
