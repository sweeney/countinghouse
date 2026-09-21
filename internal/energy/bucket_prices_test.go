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
		prices, _, _ := BucketPrices(buckets, buckets[0], stop, halfHours(start, 10, 20, 30, 40, 50, 60, 70, 80, 90, 100, 110, 120, 130, 140))
		if len(prices) != len(buckets) {
			t.Errorf("w=%v: len(prices)=%d, len(buckets)=%d", w, len(prices), len(buckets))
		}
	}
}

// A bucket the same width as a slot takes that slot's rate.
func TestBucketPricesAtSlotResolution(t *testing.T) {
	start := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)
	buckets, stop := axis(start, 30*time.Minute, 3)
	prices, basis, unpriced := BucketPrices(buckets, buckets[0], stop, halfHours(start, 0.10, 0.20, 0.30))

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
	prices, basis, unpriced := BucketPrices(buckets, buckets[0], stop, halfHours(start, 0.10, 0.20))

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
	prices, basis, unpriced := BucketPrices(buckets, buckets[0], stop, halfHours(start, 0.10, 0.30, 0.20, 0.60))

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
	prices, basis, unpriced := BucketPrices(buckets, buckets[0], stop, FlatPricer{RatePerKWh: 0.2089, known: true})

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
	prices, _, unpriced := BucketPrices(buckets, buckets[0], stop, p)

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

	prices, basis, unpriced := BucketPrices(buckets, buckets[0], stop, p)
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
	prices, _, _ := BucketPrices(buckets, buckets[0], stop, halfHours(start, 0.10, 0.40))

	want := (0.10*15 + 0.40*30) / 45
	if got := mustP(t, prices[0]); math.Abs(got-want) > 1e-9 {
		t.Errorf("prices[0] = %v, want %v (time-weighted across the boundary)", got, want)
	}
}

// No pricer at all is not free electricity.
func TestBucketPricesWithNoPricer(t *testing.T) {
	start := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)
	buckets, stop := axis(start, time.Hour, 3)
	prices, _, unpriced := BucketPrices(buckets, buckets[0], stop, nil)
	if len(prices) != 3 || unpriced != 3 {
		t.Fatalf("prices=%d unpriced=%d, want 3 and 3", len(prices), unpriced)
	}
	for i, p := range prices {
		if p != nil {
			t.Errorf("prices[%d] = %v, want null", i, *p)
		}
	}
}

// A window whose start falls INSIDE the first bucket must price that bucket over
// the part the window actually covers.
//
// The bucket axis is built on calendar boundaries, so a custom window starting
// at 00:00Z in a +01:00 zone yields a first bucket LABELLED 23:00Z the previous
// day — an hour before the window begins. kwh and cost are clipped to the window
// and describe only the covered part; the price array was computed from the
// bucket's nominal start, which the pricer holds no rate for because it is
// outside the window the curve was built over.
//
// The result was `price: null` and `unpriced_buckets: 1` on a bucket that is
// fully priced. That inverts the field's stated contract — 0 is meant to be a
// positive assertion of completeness, so a false 1 makes the assertion worthless
// — and it puts a null beside a non-zero cost, which reads as "we charged you
// for energy at a rate we do not hold".
func TestBucketPrices_PricesTheCoveredPartOfAClippedFirstBucket(t *testing.T) {
	// Rates held only from 00:00Z onward: the window's own span.
	winStart := time.Date(2026, 6, 12, 0, 0, 0, 0, time.UTC)
	p := halfHours(winStart, 0.20, 0.20, 0.30, 0.30) // 00:00–02:00Z

	// The first bucket starts an hour BEFORE the window, as a calendar axis does.
	buckets := []time.Time{winStart.Add(-time.Hour), winStart.Add(time.Hour)}
	stop := winStart.Add(2 * time.Hour)

	prices, basis, unpriced := BucketPrices(buckets, winStart, stop, p)
	if basis != PriceBasisMeanOverBucket {
		t.Fatalf("basis = %q, want mean_over_bucket", basis)
	}
	if unpriced != 0 {
		t.Errorf("unpriced = %d, want 0: every covered half hour has a rate", unpriced)
	}
	// Bucket 0 covers 23:00Z→01:00Z, of which the window covers 00:00Z→01:00Z:
	// two half hours at 0.20.
	if got := mustP(t, prices[0]); math.Abs(got-0.20) > 1e-9 {
		t.Errorf("clipped bucket price = %v, want 0.20 (the mean over the COVERED part)", got)
	}
	if got := mustP(t, prices[1]); math.Abs(got-0.30) > 1e-9 {
		t.Errorf("second bucket price = %v, want 0.30", got)
	}
}

// The clip must not swallow a genuinely unheld rate: a bucket whose covered part
// is unpriced still reports null. Clipping narrows WHICH span is asked about; it
// must not narrow it until the question always has an answer.
func TestBucketPrices_ClippingDoesNotHideAnUnpricedSpan(t *testing.T) {
	winStart := time.Date(2026, 6, 12, 0, 0, 0, 0, time.UTC)
	// Nothing held until 01:00Z, so the window's own first hour is genuinely bare.
	p := halfHours(winStart.Add(time.Hour), 0.30, 0.30)

	buckets := []time.Time{winStart.Add(-time.Hour), winStart.Add(time.Hour)}
	stop := winStart.Add(2 * time.Hour)

	prices, _, unpriced := BucketPrices(buckets, winStart, stop, p)
	if prices[0] != nil {
		t.Errorf("bucket 0 price = %v, want null: its covered part holds no rate", *prices[0])
	}
	if unpriced != 1 {
		t.Errorf("unpriced = %d, want 1", unpriced)
	}
}

// An aligned window is unchanged: the clip is a no-op when no bucket starts
// before the window does.
func TestBucketPrices_AlignedWindowIsUnaffectedByTheClip(t *testing.T) {
	start := time.Date(2026, 6, 12, 0, 0, 0, 0, time.UTC)
	p := halfHours(start, 0.10, 0.20, 0.30, 0.40)
	buckets, stop := axis(start, time.Hour, 2)

	prices, _, unpriced := BucketPrices(buckets, start, stop, p)
	if unpriced != 0 {
		t.Fatalf("unpriced = %d, want 0", unpriced)
	}
	if got := mustP(t, prices[0]); math.Abs(got-0.15) > 1e-9 {
		t.Errorf("bucket 0 = %v, want the mean of 0.10 and 0.20", got)
	}
	if got := mustP(t, prices[1]); math.Abs(got-0.35) > 1e-9 {
		t.Errorf("bucket 1 = %v, want the mean of 0.30 and 0.40", got)
	}
}
