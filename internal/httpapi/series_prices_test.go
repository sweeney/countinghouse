package httpapi

import (
	"math"
	"net/http"
	"testing"
)

// ---------------------------------------------------------------------------
// Issue #36 friction #1 / N2: prices=true on /series.
//
// Consumers were joining /series to /prices on an RFC3339 string, which works
// only while both are 30m. The server holds both sides and resolves by
// containment, so it can always answer.
// ---------------------------------------------------------------------------

// Opt-in: an existing caller's payload does not grow.
func TestSeries_PricesAreOptIn(t *testing.T) {
	s := floorSeriesSetup(t)
	r := decodeSeries(t, doGET(t, s, "/series?window=today&group_by=house"))
	if r.Prices != nil || r.PriceUnit != "" || r.UnpricedBuckets != nil {
		t.Errorf("prices were not asked for but arrived: %+v", r)
	}
}

func TestSeries_PricesAlignToBuckets(t *testing.T) {
	s := floorSeriesSetup(t)
	r := decodeSeries(t, doGET(t, s, "/series?window=today&group_by=house&prices=true"))

	if len(r.Prices) != len(r.Buckets) {
		t.Fatalf("len(prices)=%d, len(buckets)=%d: the contract is one per bucket",
			len(r.Prices), len(r.Buckets))
	}
	// Pounds, matching cost[] and kwh[] in the same response rather than the
	// pence the /prices family speaks.
	if r.PriceUnit != "GBP/kWh" {
		t.Errorf("price_unit = %q, want GBP/kWh", r.PriceUnit)
	}
	if r.PriceVATIncluded == nil || !*r.PriceVATIncluded {
		t.Errorf("price_vat_included = %v, want true", r.PriceVATIncluded)
	}
	if r.UnpricedBuckets == nil {
		t.Fatal("unpriced_buckets must be present when prices are requested, including 0")
	}
}

// The identity a consumer will reach for, and the one place it holds exactly.
// testTariffs() is flat, so every bucket is priced at one rate and cost is a
// plain multiplication.
func TestSeries_CostEqualsKWhTimesPriceOnAFlatTariff(t *testing.T) {
	s := floorSeriesSetup(t)
	r := decodeSeries(t, doGET(t, s, "/series?window=today&group_by=house&prices=true"))

	if r.PriceBasis != "flat" {
		t.Fatalf("price_basis = %q, want flat for a fixed tariff", r.PriceBasis)
	}
	var checked int
	for _, ser := range r.Series {
		for i := range ser.KWh {
			if i >= len(r.Prices) || r.Prices[i] == nil || ser.KWh[i] == 0 {
				continue
			}
			want := ser.KWh[i] * *r.Prices[i]
			if math.Abs(ser.Cost[i]-want) > 5e-4 {
				t.Errorf("%s cost[%d] = %v, want kwh × price = %v", ser.Key, i, ser.Cost[i], want)
			}
			checked++
		}
	}
	if checked == 0 {
		t.Fatal("no priced buckets compared; the fixture proves nothing")
	}
}

// unpriced_buckets is a count of nulls, and 0 is the assertion a consumer wants:
// this window is fully priced.
func TestSeries_UnpricedBucketsCountsNulls(t *testing.T) {
	s := floorSeriesSetup(t)
	r := decodeSeries(t, doGET(t, s, "/series?window=today&group_by=house&prices=true"))

	var nulls int
	for _, p := range r.Prices {
		if p == nil {
			nulls++
		}
	}
	if *r.UnpricedBuckets != nulls {
		t.Errorf("unpriced_buckets = %d, but %d entries are null", *r.UnpricedBuckets, nulls)
	}
}

// Every interval answers. This is the N2 regression: at 15m the old client-side
// join silently matched half the buckets.
func TestSeries_PricesAtEveryInterval(t *testing.T) {
	s := floorSeriesSetup(t)
	for _, iv := range []string{"15m", "30m", "1h"} {
		r := decodeSeries(t, doGET(t, s,
			"/series?window=today&interval="+iv+"&group_by=house&prices=true"))
		if len(r.Prices) != len(r.Buckets) {
			t.Errorf("interval=%s: len(prices)=%d, len(buckets)=%d",
				iv, len(r.Prices), len(r.Buckets))
		}
		if *r.UnpricedBuckets != 0 {
			t.Errorf("interval=%s: %d unpriced buckets on a flat tariff, want 0",
				iv, *r.UnpricedBuckets)
		}
	}
}

// shape is a rendering choice and must not change what a response carries.
func TestSeries_PricesSurviveTheRowsReshape(t *testing.T) {
	s := floorSeriesSetup(t)
	r := decodeSeries(t, doGET(t, s,
		"/series?window=today&group_by=house&prices=true&shape=rows"))
	if len(r.Prices) == 0 {
		t.Fatal("shape=rows dropped prices[]")
	}
	if r.PriceUnit == "" || r.UnpricedBuckets == nil {
		t.Errorf("shape=rows dropped the price metadata: %+v", r)
	}
}

// The single-device routes share the array, so a consumer can hold one rendering
// path for both.
func TestDeviceSeries_CarriesPrices(t *testing.T) {
	s := floorSeriesSetup(t)
	for _, path := range []string{
		"/devices/winefridge/series?window=today&prices=true",
		"/devices/unmonitored/series?window=today&prices=true",
	} {
		w := doGET(t, s, path)
		if w.Code != http.StatusOK {
			t.Fatalf("%s = %d: %s", path, w.Code, w.Body.String())
		}
		r := decodeSeries(t, w)
		if len(r.Prices) != len(r.Buckets) {
			t.Errorf("%s: len(prices)=%d, len(buckets)=%d", path, len(r.Prices), len(r.Buckets))
		}
	}
}

// A malformed value is a 400 before any Influx work, like every other param here.
func TestSeries_RejectsAMalformedPricesParam(t *testing.T) {
	s := floorSeriesSetup(t)
	w := doGET(t, s, "/series?window=today&group_by=house&prices=yesplease")
	if w.Code != http.StatusBadRequest {
		t.Errorf("want 400, got %d: %s", w.Code, w.Body.String())
	}
}

// ---------------------------------------------------------------------------
// The same, on a genuinely half-hourly tariff, where price_basis earns its keep.
// ---------------------------------------------------------------------------

// A 30m bucket IS a slot, so the reported price is that slot's own rate — exact,
// not an average — and it is the archive's pence converted to pounds.
func TestSeries_HalfHourlyPricesAreSlotExact(t *testing.T) {
	s, buckets, rates := scSetup(t, len(scBuckets(t)))

	r := decodeSeries(t, doGET(t, s,
		"/series?window=today&interval=30m&group_by=house&prices=true"))

	if r.PriceBasis != "slot" {
		t.Fatalf("price_basis = %q, want slot at interval=30m", r.PriceBasis)
	}
	if *r.UnpricedBuckets != 0 {
		t.Errorf("unpriced_buckets = %d, want 0: every slot is held", *r.UnpricedBuckets)
	}
	if len(r.Prices) != len(buckets) {
		t.Fatalf("len(prices)=%d, len(buckets)=%d", len(r.Prices), len(buckets))
	}
	for i := range r.Prices {
		if r.Prices[i] == nil {
			t.Fatalf("prices[%d] is null but the slot is held", i)
		}
		// The archive stores pence inc-VAT; the array speaks pounds.
		if want := rates[i] / 100; math.Abs(*r.Prices[i]-want) > 1e-9 {
			t.Errorf("prices[%d] = %v, want %v", i, *r.Prices[i], want)
		}
	}
}

// A window the archive only partly covers reports nulls and counts them, rather
// than charging nothing for the gap.
func TestSeries_HalfHourlyPartialCoverageReportsNulls(t *testing.T) {
	all := len(scBuckets(t))
	s, _, _ := scSetup(t, all-4)

	r := decodeSeries(t, doGET(t, s,
		"/series?window=today&interval=30m&group_by=house&prices=true"))

	if *r.UnpricedBuckets != 4 {
		t.Errorf("unpriced_buckets = %d, want 4", *r.UnpricedBuckets)
	}
	for i := all - 4; i < all && i < len(r.Prices); i++ {
		if r.Prices[i] != nil {
			t.Errorf("prices[%d] = %v, want null: no slot is held", i, *r.Prices[i])
		}
	}
}

// A bucket spanning two slots reports their time-weighted mean, and says so.
func TestSeries_CoarseBucketsReportAMeanBasis(t *testing.T) {
	s, _, rates := scSetup(t, len(scBuckets(t)))

	r := decodeSeries(t, doGET(t, s,
		"/series?window=today&interval=1h&group_by=house&prices=true"))

	if r.PriceBasis != "mean_over_bucket" {
		t.Fatalf("price_basis = %q, want mean_over_bucket at interval=1h", r.PriceBasis)
	}
	if r.Prices[0] == nil {
		t.Fatal("prices[0] is null but both slots are held")
	}
	// The first hour covers the first two half hours in equal measure.
	want := (rates[0] + rates[1]) / 2 / 100
	if math.Abs(*r.Prices[0]-want) > 1e-9 {
		t.Errorf("prices[0] = %v, want the time-weighted mean %v", *r.Prices[0], want)
	}
}

// tariff_codes names the tariff behind the window, so a consumer comparing two
// periods can tell whether it is comparing like with like.
func TestSeries_ReportsTheTariffCode(t *testing.T) {
	s, _, _ := scSetup(t, len(scBuckets(t)))
	r := decodeSeries(t, doGET(t, s,
		"/series?window=today&interval=30m&group_by=house&prices=true"))
	if len(r.TariffCodes) != 1 || r.TariffCodes[0] != pxTariff {
		t.Errorf("tariff_codes = %v, want [%s]", r.TariffCodes, pxTariff)
	}
}

// A custom window whose start falls inside the first bucket must still price
// that bucket.
//
// The bucket axis is built on calendar boundaries, so a window starting at
// 01:15Z at interval=1h yields a first bucket LABELLED 01:00Z — before the
// window. kwh and cost are clipped to the window and describe only the covered
// part; the price array was computed from the bucket's nominal start, which the
// curve holds no rate for because it was built over the window.
//
// The result was `price: null` and `unpriced_buckets: 1` on a fully priced
// bucket — a null sitting beside a non-zero cost, and a count whose documented
// meaning is "0 asserts the window is complete".
func TestSeries_PricesAClippedFirstBucket(t *testing.T) {
	s, _, _ := scSetup(t, len(scBuckets(t)))

	r := decodeSeries(t, doGET(t, s,
		"/series?window=custom&from=2026-06-11T01:15:00Z&to=2026-06-11T04:00:00Z"+
			"&interval=1h&group_by=house&prices=true"))

	if len(r.Prices) != len(r.Buckets) {
		t.Fatalf("len(prices)=%d, len(buckets)=%d", len(r.Prices), len(r.Buckets))
	}
	if *r.UnpricedBuckets != 0 {
		t.Errorf("unpriced_buckets = %d, want 0: the archive covers this whole window",
			*r.UnpricedBuckets)
	}
	if r.Prices[0] == nil {
		t.Error("prices[0] is null, but the part of that bucket the window covers is priced")
	}
}
