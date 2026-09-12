package prices

import (
	"math"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// The forward curve: what the price endpoints are built on.
//
// This is the half of the project that changes behaviour rather than recording
// it. The archive answers "what did it cost"; the curve answers "when is it
// cheap", which is the only question that can move a kWh to a different hour.
//
// Two things it must get right, and both are about not lying by omission:
// CONTIGUITY (a run of slots spanning a gap is not a run — you cannot schedule
// across prices we do not hold) and BANDS that respect spread, so a flat day does
// not report a third of itself as a peak.
// ---------------------------------------------------------------------------

// curveAt builds a contiguous half-hourly curve from prices given in order,
// starting at the given instant. A nil entry leaves a GAP — no slot at all.
func curveAt(t *testing.T, start string, excPence []*float64) []Slot {
	t.Helper()
	from := at(t, start)
	var out []Slot
	for i, p := range excPence {
		if p == nil {
			continue
		}
		s := from.Add(time.Duration(i) * 30 * time.Minute)
		e := s.Add(30 * time.Minute)
		out = append(out, Slot{
			TariffCode: tariffA, ValidFrom: s, ValidTo: &e,
			ExcVATPence: *p, IncVATPence: *p * 1.05, RetrievedAt: from,
		})
	}
	return out
}

func pp(v float64) *float64 { return &v }

// ---------------------------------------------------------------------------
// Summary
// ---------------------------------------------------------------------------

func TestCurveSummary(t *testing.T) {
	c := Curve{Slots: curveAt(t, "2026-09-11T00:00:00Z",
		[]*float64{pp(10), pp(20), pp(30), pp(40)})}

	got := c.Summary()
	if got.Slots != 4 {
		t.Errorf("slots = %d, want 4", got.Slots)
	}
	// Reported inc-VAT, because that is what a consumer is actually charged and a
	// dashboard showing ex-VAT prices would understate every number on screen.
	if !approxEq(got.Min, 10*1.05) || !approxEq(got.Max, 40*1.05) {
		t.Errorf("min/max = %v/%v, want %v/%v", got.Min, got.Max, 10*1.05, 40*1.05)
	}
	if !approxEq(got.Mean, 25*1.05) {
		t.Errorf("mean = %v, want %v", got.Mean, 25*1.05)
	}
}

func TestCurveSummaryOfAnEmptyCurve(t *testing.T) {
	got := Curve{}.Summary()
	if got.Slots != 0 {
		t.Errorf("slots = %d", got.Slots)
	}
	// Not NaN. An empty curve's mean is undefined, and NaN would serialise as null
	// or break a chart; zero with Slots==0 is unambiguous to a consumer that looks.
	if math.IsNaN(got.Mean) || math.IsNaN(got.Min) || math.IsNaN(got.Max) {
		t.Errorf("summary of an empty curve must not be NaN: %+v", got)
	}
}

// ---------------------------------------------------------------------------
// Bands
// ---------------------------------------------------------------------------

// Bands are relative to the window, because "cheap" only means anything compared
// with the alternatives. They are NOT terciles: a tercile split always calls a
// third of the day a peak, which on a flat day is simply false and on a volatile
// one understates how unusual the real peak is.
func TestCurveBandsRespectSpread(t *testing.T) {
	t.Run("a volatile day gets the full range of bands", func(t *testing.T) {
		// Values deliberately placed below, AT and above the mean. An earlier
		// fixture had nothing near the mean, so "normal" was unreachable by
		// construction and the test was asserting something impossible.
		c := Curve{Slots: curveAt(t, "2026-09-11T00:00:00Z",
			[]*float64{pp(5), pp(10), pp(25), pp(33), pp(35), pp(60), pp(80)})}
		bands := map[Band]int{}
		for _, s := range c.Priced() {
			bands[c.BandOf(s)]++
		}
		if bands[BandCheap] == 0 || bands[BandPeak] == 0 || bands[BandNormal] == 0 {
			t.Errorf("expected all three bands on a volatile day, got %v", bands)
		}
	})

	t.Run("a flat day is all normal", func(t *testing.T) {
		c := Curve{Slots: curveAt(t, "2026-09-11T00:00:00Z",
			[]*float64{pp(25), pp(25), pp(25), pp(25)})}
		for _, s := range c.Priced() {
			if b := c.BandOf(s); b != BandNormal {
				t.Errorf("slot at %s banded %q on a flat day; nothing is a peak when "+
					"every slot costs the same", s.ValidFrom.Format(time.RFC3339), b)
			}
		}
	})

	t.Run("a negative price is its own band", func(t *testing.T) {
		// Being PAID to consume is categorically different from merely cheap, and it
		// is the signal most worth surfacing — so it is not folded into "cheap".
		c := Curve{Slots: curveAt(t, "2026-09-11T00:00:00Z",
			[]*float64{pp(-5), pp(10), pp(25), pp(60)})}
		if got := c.BandOf(c.Priced()[0]); got != BandPlunge {
			t.Errorf("a negative price banded %q, want %q", got, BandPlunge)
		}
	})

	t.Run("exactly zero is a plunge too", func(t *testing.T) {
		// 0.00p is free energy. Treating it as merely "cheap" would bury the one
		// slot a consumer would most want to schedule into.
		c := Curve{Slots: curveAt(t, "2026-09-11T00:00:00Z",
			[]*float64{pp(0), pp(25), pp(50)})}
		if got := c.BandOf(c.Priced()[0]); got != BandPlunge {
			t.Errorf("a zero price banded %q, want %q", got, BandPlunge)
		}
	})
}

// Rank and percentile are served so two dashboards agree, and so a consumer that
// dislikes our banding can derive its own.
func TestCurveRankAndPercentile(t *testing.T) {
	c := Curve{Slots: curveAt(t, "2026-09-11T00:00:00Z",
		[]*float64{pp(40), pp(10), pp(30), pp(20)})}

	priced := c.Priced()
	ranks := map[float64]int{}
	for _, s := range priced {
		ranks[s.ExcVATPence] = c.RankOf(s)
	}
	// 1 is the CHEAPEST, which is the way round a consumer expects when the point
	// is finding a good time to run something.
	if ranks[10] != 1 || ranks[20] != 2 || ranks[30] != 3 || ranks[40] != 4 {
		t.Errorf("ranks = %v, want cheapest=1", ranks)
	}
	for _, s := range priced {
		p := c.PercentileOf(s)
		if p < 0 || p > 1 {
			t.Errorf("percentile %v out of range for %v", p, s.ExcVATPence)
		}
	}
	if c.PercentileOf(priced[1]) != 0 { // the 10p slot, cheapest
		t.Errorf("the cheapest slot's percentile = %v, want 0", c.PercentileOf(priced[1]))
	}
}

// ---------------------------------------------------------------------------
// Cheapest contiguous run
// ---------------------------------------------------------------------------

func TestCheapestRun(t *testing.T) {
	// 00:00 .. 04:00, eight slots. The cheap stretch is 02:00-03:00.
	c := Curve{Slots: curveAt(t, "2026-09-11T00:00:00Z", []*float64{
		pp(30), pp(28), pp(25), pp(10), pp(8), pp(26), pp(40), pp(45),
	})}

	for _, tc := range []struct {
		name     string
		duration time.Duration
		wantFrom string
		wantMean float64
	}{
		{
			name:     "half an hour picks the single cheapest slot",
			duration: 30 * time.Minute,
			wantFrom: "2026-09-11T02:00:00Z", wantMean: 8,
		},
		{
			name:     "an hour picks the cheapest adjacent pair",
			duration: time.Hour,
			wantFrom: "2026-09-11T01:30:00Z", wantMean: 9,
		},
		{
			// 90 minutes spans three slots. 01:00-02:30 is (25+10+8)/3 = 14.33,
			// against 01:30-03:00 at (10+8+26)/3 = 14.67.
			name:     "ninety minutes picks the cheapest triple",
			duration: 90 * time.Minute,
			wantFrom: "2026-09-11T01:00:00Z", wantMean: (25 + 10 + 8) / 3.0,
		},
		{
			// A duration that is not a multiple of the slot length rounds UP to
			// whole slots: you cannot buy a third of a slot at its price, and
			// rounding down would quote a window too short to finish the job.
			name:     "forty minutes needs two slots",
			duration: 40 * time.Minute,
			wantFrom: "2026-09-11T01:30:00Z", wantMean: 9,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run, ok := c.CheapestRun(tc.duration, time.Time{})
			if !ok {
				t.Fatal("no run found")
			}
			if !run.From.Equal(at(t, tc.wantFrom)) {
				t.Errorf("from = %s, want %s", run.From.Format(time.RFC3339), tc.wantFrom)
			}
			if !approxEq(run.MeanExcVATPence, tc.wantMean) {
				t.Errorf("mean = %v, want %v", run.MeanExcVATPence, tc.wantMean)
			}
			if run.To.Sub(run.From) < tc.duration {
				t.Errorf("run [%s,%s) is shorter than the %v asked for",
					run.From.Format(time.RFC3339), run.To.Format(time.RFC3339), tc.duration)
			}
		})
	}
}

// A run may not span a gap. Slots either side of missing prices are not adjacent
// in any useful sense: you cannot schedule across half hours whose price we do
// not know, and pretending otherwise would quote a cheap window that might not be.
func TestCheapestRunWillNotSpanAGap(t *testing.T) {
	// Two very cheap slots, separated by a missing one.
	c := Curve{Slots: curveAt(t, "2026-09-11T00:00:00Z", []*float64{
		pp(5), nil, pp(5), pp(30), pp(31), pp(32),
	})}

	run, ok := c.CheapestRun(time.Hour, time.Time{})
	if !ok {
		t.Fatal("an hour should be available among the contiguous slots")
	}
	if run.From.Before(at(t, "2026-09-11T01:00:00Z")) {
		t.Errorf("run starts %s — it has bridged the 00:30 gap",
			run.From.Format(time.RFC3339))
	}
	// This is the assertion that actually proves it. The two 5p slots sit either
	// side of the gap, so BRIDGING would have yielded a mean of 5.0 — far cheaper
	// than anything legitimately available. Getting 17.5 (the contiguous 5p+30p
	// pair at 01:00) is only possible if the gap was respected.
	if !approxEq(run.MeanExcVATPence, 17.5) {
		t.Errorf("mean = %v, want 17.5; a mean of 5 would mean it bridged the gap",
			run.MeanExcVATPence)
	}
}

func TestCheapestRunRespectsADeadline(t *testing.T) {
	c := Curve{Slots: curveAt(t, "2026-09-11T00:00:00Z", []*float64{
		pp(30), pp(30), pp(30), pp(30), pp(5), pp(5),
	})}

	// Without a deadline the cheap pair at 02:00 wins.
	run, ok := c.CheapestRun(time.Hour, time.Time{})
	if !ok || !run.From.Equal(at(t, "2026-09-11T02:00:00Z")) {
		t.Fatalf("unbounded run = %+v, want the 02:00 pair", run)
	}

	// It must FINISH by the deadline, not merely start before it — a load that
	// overruns into unpriced or expensive time was not scheduled, it was guessed.
	run, ok = c.CheapestRun(time.Hour, at(t, "2026-09-11T02:30:00Z"))
	if !ok {
		t.Fatal("a run should still be available before the deadline")
	}
	if run.To.After(at(t, "2026-09-11T02:30:00Z")) {
		t.Errorf("run ends %s, after the 02:30 deadline", run.To.Format(time.RFC3339))
	}
}

func TestCheapestRunWhenNothingFits(t *testing.T) {
	c := Curve{Slots: curveAt(t, "2026-09-11T00:00:00Z", []*float64{pp(20), pp(21)})}

	if _, ok := c.CheapestRun(6*time.Hour, time.Time{}); ok {
		t.Error("six hours cannot fit in one hour of prices; want not-ok")
	}
	if _, ok := (Curve{}).CheapestRun(30*time.Minute, time.Time{}); ok {
		t.Error("an empty curve has no cheapest run")
	}
	if _, ok := c.CheapestRun(0, time.Time{}); ok {
		t.Error("a zero duration is meaningless; want not-ok rather than an empty run")
	}
}

// Negative prices must win, not be skipped by any abs() or positivity assumption.
func TestCheapestRunPrefersNegativePrices(t *testing.T) {
	c := Curve{Slots: curveAt(t, "2026-09-11T00:00:00Z", []*float64{
		pp(20), pp(-6), pp(-4), pp(20),
	})}
	run, ok := c.CheapestRun(time.Hour, time.Time{})
	if !ok {
		t.Fatal("no run")
	}
	if !run.From.Equal(at(t, "2026-09-11T00:30:00Z")) {
		t.Errorf("from = %s, want the negative pair at 00:30", run.From.Format(time.RFC3339))
	}
	if run.MeanExcVATPence >= 0 {
		t.Errorf("mean = %v, want negative — being paid to consume is the cheapest case",
			run.MeanExcVATPence)
	}
}

// ---------------------------------------------------------------------------
// Gaps
// ---------------------------------------------------------------------------

// A window with prices missing must SAY so. An endpoint that returned 46 slots for
// a 48-slot request and left the consumer to notice is the silent-gap failure in a
// new place.
func TestCurveMissingSlots(t *testing.T) {
	from, to := at(t, "2026-09-11T00:00:00Z"), at(t, "2026-09-11T03:00:00Z")
	c := Curve{
		From: from, To: to,
		Slots: curveAt(t, "2026-09-11T00:00:00Z", []*float64{
			pp(20), pp(21), nil, nil, pp(24), pp(25),
		}),
	}

	missing := c.Missing()
	if len(missing) != 2 {
		t.Fatalf("missing = %v, want the two absent slots", missing)
	}
	if !missing[0].Equal(at(t, "2026-09-11T01:00:00Z")) ||
		!missing[1].Equal(at(t, "2026-09-11T01:30:00Z")) {
		t.Errorf("missing = %v, want 01:00 and 01:30", missing)
	}
	if c.Complete() {
		t.Error("a curve with gaps must not report complete")
	}
}

func TestCurveCompleteWhenFull(t *testing.T) {
	from, to := at(t, "2026-09-11T00:00:00Z"), at(t, "2026-09-11T02:00:00Z")
	c := Curve{From: from, To: to, Slots: curveAt(t, "2026-09-11T00:00:00Z",
		[]*float64{pp(20), pp(21), pp(22), pp(23)})}
	if len(c.Missing()) != 0 {
		t.Errorf("missing = %v, want none", c.Missing())
	}
	if !c.Complete() {
		t.Error("a full curve should report complete")
	}
}

// ---------------------------------------------------------------------------
// Stats
// ---------------------------------------------------------------------------

// Per-day aggregates, for a dashboard showing how volatile the tariff has been.
func TestCurveDailyStats(t *testing.T) {
	loc := londonTZ(t)
	// Two local days: the first cheap and calm, the second volatile with a plunge.
	var slots []Slot
	slots = append(slots, curveAt(t, "2026-09-09T23:00:00Z",
		[]*float64{pp(20), pp(21), pp(22)})...) // local 10 Sep
	slots = append(slots, curveAt(t, "2026-09-10T23:00:00Z",
		[]*float64{pp(-3), pp(10), pp(70)})...) // local 11 Sep

	days := Curve{Slots: slots}.DailyStats(loc)
	if len(days) != 2 {
		t.Fatalf("got %d days, want 2: %+v", len(days), days)
	}
	if days[0].Day != "2026-09-10" || days[1].Day != "2026-09-11" {
		t.Errorf("days = %q, %q", days[0].Day, days[1].Day)
	}
	if days[0].PlungeSlots != 0 {
		t.Errorf("day 1 plunge slots = %d, want 0", days[0].PlungeSlots)
	}
	if days[1].PlungeSlots != 1 {
		t.Errorf("day 2 plunge slots = %d, want 1", days[1].PlungeSlots)
	}
	// Spread is the number that says whether shifting load is worth the bother.
	if days[1].SpreadExcVATPence <= days[0].SpreadExcVATPence {
		t.Errorf("the volatile day's spread (%v) should exceed the calm one's (%v)",
			days[1].SpreadExcVATPence, days[0].SpreadExcVATPence)
	}
}

func approxEq(a, b float64) bool { return math.Abs(a-b) < 1e-9 }
