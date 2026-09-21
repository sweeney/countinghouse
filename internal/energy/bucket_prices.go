package energy

import "time"

// ---------------------------------------------------------------------------
// The price behind each bucket (issue #36, friction #1).
//
// /series already returns `cost`, but cost is kwh × price — and you cannot
// recover the price from a bucket where kwh is zero. Those are exactly the
// buckets that answer "it was cheap and we did NOT use it", which is the question
// a load-shifting analysis is actually asking.
//
// So consumers were joining /series to /prices by matching an RFC3339 bucket
// label against a slot's valid_from. That works only while both happen to be 30m
// and render local offsets identically. At interval=15m it silently dropped every
// odd bucket — half the energy, and not a random half: the half of the day that
// starts on the hour.
//
// The server never had that problem. A 15m bucket lies INSIDE exactly one half
// hour, and Curve.RateAt already resolves by containment (it truncates to the
// slot grid and checks Covers). Containment is unambiguous; string equality is
// not. This file exposes what the cost path already knew.
// ---------------------------------------------------------------------------

// Price bases, reported on the wire so a consumer knows what a value MEANS
// rather than inferring it from the interval they asked for.
const (
	// PriceBasisSlot: the bucket sits inside one rate interval, so the value is
	// that interval's own rate — exact, not an average.
	PriceBasisSlot = "slot"

	// PriceBasisMeanOverBucket: the bucket spans several rate intervals, so the
	// value is the TIME-weighted mean across them.
	PriceBasisMeanOverBucket = "mean_over_bucket"

	// PriceBasisFlat: one rate covers all time, repeated per bucket.
	PriceBasisFlat = "flat"
)

// PriceUnitGBPPerKWh is the unit of every value in a prices[] array.
//
// Pounds, not the pence the /prices family speaks, because this array lives
// beside `cost` and `kwh` in the same response and `cost ≈ kwh × price` should
// read directly. Consistency INSIDE one payload beats consistency with a
// different endpoint — and price_unit states it either way, so a consumer
// porting from the old join meets a factor of 100, which is loud, not silent.
const PriceUnitGBPPerKWh = "GBP/kWh"

// BucketPrices returns the VAT-inclusive £/kWh behind each bucket, the basis
// those values were computed on, and how many buckets no rate is held for.
//
// stop is the window end, needed because the bucket axis carries STARTS only and
// the last bucket's length cannot otherwise be known. Bucket i runs to
// buckets[i+1], which is also what makes a 23- or 25-hour DST day weight
// correctly without special-casing.
//
// A nil entry means NO RATE IS HELD, never free — the same distinction
// Pricer.RateAt's bool carries, preserved rather than flattened to zero. The
// returned count is of those nils, so 0 is a positive assertion that the whole
// window is priced.
//
// len(out) == len(buckets), always. That is the contract consumers zip against.
func BucketPrices(buckets []time.Time, stop time.Time, p Pricer) (prices []*float64, basis string, unpriced int) {
	prices = make([]*float64, len(buckets))
	if p == nil {
		// No pricer is not a flat rate of zero. Every bucket is unknown, and the
		// basis is the one that does not imply an average was taken.
		return prices, PriceBasisFlat, len(buckets)
	}

	ri := rateInterval(p)
	basis = basisFor(buckets, stop, ri)

	for i, start := range buckets {
		end := bucketEnd(buckets, stop, i)
		var (
			rate float64
			ok   bool
		)
		if basis == PriceBasisMeanOverBucket {
			rate, ok = meanRateOver(start, end, ri, p)
		} else {
			// The bucket lies inside one rate interval (or the rate never changes),
			// so its start instant identifies the rate exactly.
			rate, ok = p.RateAt(start)
		}
		if !ok {
			unpriced++
			continue
		}
		v := rate
		prices[i] = &v
	}
	return prices, basis, unpriced
}

// rateInterval reports the finest interval p's rate changes at, or 0 when it
// never changes within a window. Optional on Pricer by design — see Granularity.
func rateInterval(p Pricer) time.Duration {
	g, ok := p.(Granularity)
	if !ok {
		return 0
	}
	return g.RateInterval()
}

// basisFor decides how the values were arrived at, from the axis and the rate
// grid alone. A single bucket longer than one rate interval is enough to make the
// whole array a set of means: reporting a mixed basis per bucket would give a
// consumer an array whose entries mean different things.
func basisFor(buckets []time.Time, stop time.Time, ri time.Duration) string {
	if ri <= 0 {
		return PriceBasisFlat
	}
	for i := range buckets {
		if bucketEnd(buckets, stop, i).Sub(buckets[i]) > ri {
			return PriceBasisMeanOverBucket
		}
	}
	return PriceBasisSlot
}

// bucketEnd returns the exclusive end of bucket i: the next bucket's start, or
// the window stop for the last one.
func bucketEnd(buckets []time.Time, stop time.Time, i int) time.Time {
	if i+1 < len(buckets) {
		return buckets[i+1]
	}
	return stop
}

// meanRateOver returns the TIME-weighted mean rate across [start, end).
//
// Time-weighted, deliberately, not energy-weighted. Energy-weighted is cost÷kwh,
// which is undefined in a zero-kWh bucket — and those are the buckets that answer
// "it was cheap and we did not use it". A time-weighted mean is a property of the
// tariff, defined regardless of what was consumed.
//
// It reports unknown if ANY interval inside the bucket is unpriced. A mean over
// the slots that happen to be held is a plausible-looking wrong number wearing
// the shape of a right one, which is the failure this service refuses everywhere
// else: a visible gap beats a plausible total.
func meanRateOver(start, end time.Time, ri time.Duration, p Pricer) (float64, bool) {
	var weighted, total float64
	for t := start; t.Before(end); {
		// Step to the end of the rate interval CONTAINING t, not t+ri: a bucket
		// whose start is off the grid (from=14:29 at interval=30m yields a 14:00
		// bucket) would otherwise sample each step at an instant straddling two
		// slots, and weight both by the wrong amount.
		next := t.UTC().Truncate(ri).Add(ri)
		if !next.After(t) {
			next = t.Add(ri) // unreachable for ri > 0; cheaper than trusting that
		}
		if next.After(end) {
			next = end
		}
		rate, ok := p.RateAt(t)
		if !ok {
			return 0, false
		}
		w := next.Sub(t).Seconds()
		weighted += rate * w
		total += w
		t = next
	}
	if total == 0 {
		return 0, false
	}
	return weighted / total, true
}
