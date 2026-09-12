package energy

import (
	"encoding/json"
	"math"
	"os"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Costing against a REAL mixed-sign day.
//
// The fixture is one published local day — 48 slots, 10 of them negative, a
// 50.33p spread from -2.50p to 47.83p. Synthetic fixtures are too tidy for this:
// the arithmetic that matters is what happens when a bill contains slots of BOTH
// signs, because that is where a stray abs(), clamp, or "costs are positive"
// assumption hides. A day of 20p slots would never catch one.
//
// Read from internal/octopus/testdata so there is ONE copy of the recording
// rather than a duplicate that can drift from it.
// ---------------------------------------------------------------------------

const realDayFixture = "../octopus/testdata/unit_rates_mixed_sign_day.json"

// realDay loads the fixture as a pricer plus its slot starts, oldest first.
func realDay(t *testing.T) (Pricer, []time.Time, []float64) {
	t.Helper()
	raw, err := os.ReadFile(realDayFixture)
	if err != nil {
		t.Skipf("fixture unavailable: %v", err)
	}
	var doc struct {
		Results []struct {
			ValueExcVAT float64 `json:"value_exc_vat"`
			ValueIncVAT float64 `json:"value_inc_vat"`
			ValidFrom   string  `json:"valid_from"`
		} `json:"results"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Results) != 48 {
		t.Fatalf("fixture has %d slots, want a full 48-slot day", len(doc.Results))
	}

	type row struct {
		at  time.Time
		inc float64
	}
	rows := make([]row, 0, len(doc.Results))
	for _, r := range doc.Results {
		at, err := time.Parse(time.RFC3339, r.ValidFrom)
		if err != nil {
			t.Fatal(err)
		}
		rows = append(rows, row{at.UTC(), r.ValueIncVAT})
	}
	// The API answers newest-first; costing needs ascending.
	for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
		rows[i], rows[j] = rows[j], rows[i]
	}

	starts := make([]time.Time, 0, len(rows))
	ratesPence := make([]float64, 0, len(rows))
	rates := make([]*float64, 0, len(rows))
	for i := range rows {
		starts = append(starts, rows[i].at)
		ratesPence = append(ratesPence, rows[i].inc)
		v := rows[i].inc
		rates = append(rates, &v)
	}
	return slotRates{start: starts[0], rates: rates}, starts, ratesPence
}

// The fixture must actually contain what the comment claims, or every assertion
// below is testing a tidier day than it thinks.
func TestRealDayFixtureHasBothSigns(t *testing.T) {
	_, _, pence := realDay(t)

	var neg, pos int
	lo, hi := pence[0], pence[0]
	for _, p := range pence {
		if p < 0 {
			neg++
		} else {
			pos++
		}
		lo = math.Min(lo, p)
		hi = math.Max(hi, p)
	}
	t.Logf("fixture: %d slots, %d negative, %d positive, %.2fp..%.2fp (inc VAT)",
		len(pence), neg, pos, lo, hi)
	if neg == 0 {
		t.Fatal("no negative slots: the fixture cannot test mixed-sign costing")
	}
	if pos == 0 {
		t.Fatal("no positive slots either")
	}
	if hi-lo < 20 {
		t.Errorf("spread is only %.2fp; too flat to exercise shifting", hi-lo)
	}
}

// A steady load across the whole day must cost exactly the sum of kWh × that
// slot's rate. Computed independently here from the fixture rather than by calling
// the same helper twice, so the test can actually disagree with the code.
func TestRealDaySteadyLoadCost(t *testing.T) {
	pricer, starts, pence := realDay(t)

	const perSlot = 0.0335 // ~67 W continuous, a fridge
	kwh := make([]float64, len(starts))
	for i := range kwh {
		kwh[i] = perSlot
	}

	cost, unpriced := CostBuckets(starts, kwh, pricer)
	if unpriced != 0 {
		t.Errorf("unpriced = %v; the fixture is a complete day", unpriced)
	}

	var want float64
	for _, p := range pence {
		want += perSlot * p / 100
	}
	if math.Abs(cost-want) > 1e-9 {
		t.Errorf("cost = %.6f, want %.6f", cost, want)
	}

	// A steady load's effective rate is the unweighted mean, which is the property
	// the per-device attribution decision rests on.
	var mean float64
	for _, p := range pence {
		mean += p
	}
	mean /= float64(len(pence))
	eff := EffectiveRate(cost, perSlot*float64(len(starts))) * 100
	if math.Abs(eff-mean) > 1e-6 {
		t.Errorf("effective rate %.4fp != mean %.4fp for a steady load", eff, mean)
	}
	t.Logf("steady load: %.4f kWh cost £%.4f at %.2fp/kWh", perSlot*float64(len(starts)), cost, eff)
}

// The case a synthetic all-positive fixture can never reach: a load that runs ONLY
// in negative slots must produce a NEGATIVE cost. Being paid to consume is the
// tariff working, and any clamp or abs() on the way through would turn a credit
// into a charge.
func TestRealDayLoadInNegativeSlotsIsPaid(t *testing.T) {
	pricer, starts, pence := realDay(t)

	kwh := make([]float64, len(starts))
	var negKWh, wantCost float64
	for i, p := range pence {
		if p < 0 {
			kwh[i] = 1.5 // a dishwasher's worth, in each negative slot
			negKWh += 1.5
			wantCost += 1.5 * p / 100
		}
	}
	if negKWh == 0 {
		t.Fatal("fixture has no negative slots")
	}

	cost, unpriced := CostBuckets(starts, kwh, pricer)
	if unpriced != 0 {
		t.Errorf("unpriced = %v", unpriced)
	}
	if cost >= 0 {
		t.Errorf("cost = %v; consuming only in negative slots must be a CREDIT", cost)
	}
	if math.Abs(cost-wantCost) > 1e-9 {
		t.Errorf("cost = %.6f, want %.6f", cost, wantCost)
	}
	if eff := EffectiveRate(cost, negKWh); eff >= 0 {
		t.Errorf("effective rate = %v, want negative", eff)
	}
	t.Logf("%.1f kWh entirely in negative slots: £%.4f (a credit) at %.2fp/kWh",
		negKWh, cost, EffectiveRate(cost, negKWh)*100)
}

// Mixed signs must NET, not accumulate in absolute terms. This is the assertion
// that catches a sign error anywhere in the chain: the same energy split between
// the day's cheapest and dearest slots has to come out as the difference, and a
// bill that summed magnitudes would be wildly higher.
func TestRealDayMixedSignsNet(t *testing.T) {
	pricer, starts, pence := realDay(t)

	cheapest, dearest := 0, 0
	for i, p := range pence {
		if p < pence[cheapest] {
			cheapest = i
		}
		if p > pence[dearest] {
			dearest = i
		}
	}
	if pence[cheapest] >= 0 {
		t.Fatal("the cheapest slot is not negative; fixture cannot test netting")
	}

	kwh := make([]float64, len(starts))
	kwh[cheapest] = 10
	kwh[dearest] = 10

	cost, _ := CostBuckets(starts, kwh, pricer)
	want := 10*pence[cheapest]/100 + 10*pence[dearest]/100
	if math.Abs(cost-want) > 1e-9 {
		t.Errorf("cost = %.6f, want %.6f (the NET of a credit and a charge)", cost, want)
	}
	// A magnitude-summing bug would produce this instead.
	magnitudes := 10*math.Abs(pence[cheapest])/100 + 10*math.Abs(pence[dearest])/100
	if math.Abs(cost-magnitudes) < 1e-9 {
		t.Error("cost equals the sum of MAGNITUDES; signs are being discarded")
	}
	t.Logf("10 kWh at %.2fp and 10 kWh at %.2fp: net £%.4f (magnitudes would give £%.4f)",
		pence[cheapest], pence[dearest], cost, magnitudes)
}

// The whole point of the tariff, measured on a real day: moving a deferrable load
// from the dearest stretch to the cheapest must reduce the bill, and here it turns
// a charge into a credit.
func TestRealDayShiftingALoadSaves(t *testing.T) {
	pricer, starts, pence := realDay(t)

	// Three hours — six slots — of a 1 kWh/slot load.
	const run = 6
	best, worst := 0, 0
	bestSum, worstSum := math.Inf(1), math.Inf(-1)
	for i := 0; i+run <= len(pence); i++ {
		var sum float64
		for j := i; j < i+run; j++ {
			sum += pence[j]
		}
		if sum < bestSum {
			bestSum, best = sum, i
		}
		if sum > worstSum {
			worstSum, worst = sum, i
		}
	}

	at := func(start int) float64 {
		kwh := make([]float64, len(starts))
		for j := start; j < start+run; j++ {
			kwh[j] = 1
		}
		c, _ := CostBuckets(starts, kwh, pricer)
		return c
	}
	cheap, dear := at(best), at(worst)

	if cheap >= dear {
		t.Errorf("shifting to the cheapest window cost %.4f, not less than %.4f", cheap, dear)
	}
	t.Logf("6 kWh run: cheapest window £%.4f (from %s) vs dearest £%.4f (from %s) — saving £%.4f",
		cheap, starts[best].Format("15:04"), dear, starts[worst].Format("15:04"), dear-cheap)
	if cheap > 0 {
		t.Logf("  (note: the cheapest window is still a net charge on this day)")
	} else {
		t.Logf("  (the cheapest window is a net CREDIT)")
	}
}

// A gap in the middle of a real day must surface as unpriced energy, with the
// priced remainder still costed correctly — not the whole day voided, and not the
// gap silently free.
func TestRealDayWithAGapReportsUnpriced(t *testing.T) {
	_, starts, pence := realDay(t)

	// Punch a two-slot hole.
	rates := make([]*float64, len(pence))
	for i := range pence {
		if i == 20 || i == 21 {
			continue
		}
		v := pence[i]
		rates[i] = &v
	}
	pricer := slotRates{start: starts[0], rates: rates}

	kwh := make([]float64, len(starts))
	for i := range kwh {
		kwh[i] = 0.5
	}

	cost, unpriced := CostBuckets(starts, kwh, pricer)
	if math.Abs(unpriced-1.0) > 1e-9 {
		t.Errorf("unpriced = %v, want 1.0 kWh (two slots at 0.5)", unpriced)
	}
	var want float64
	for i, p := range pence {
		if i == 20 || i == 21 {
			continue
		}
		want += 0.5 * p / 100
	}
	if math.Abs(cost-want) > 1e-9 {
		t.Errorf("cost = %.6f, want %.6f — the priced remainder must still be correct", cost, want)
	}
}
