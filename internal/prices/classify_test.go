package prices

import (
	"fmt"
	"math"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Classify must agree with the per-slot methods, exactly, on every curve shape
// that can reach it.
//
// This is a characterisation test, not a behaviour change: Classify exists only
// to stop the rendering loop being quadratic, so the ONE thing it must not do is
// answer differently from the methods it replaces. Ties, an all-plunge window and
// a single-slot window are each a case where a faster implementation is easy to
// get subtly wrong — a rank that counts <= instead of <, a percentile that divides
// by n instead of n-1, a median recomputed over a filtered set.
// ---------------------------------------------------------------------------

// curveFrom builds a curve of contiguous half-hourly slots at the given
// inc-VAT pence prices, starting at a fixed instant.
func curveFrom(pence ...float64) Curve {
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	slots := make([]Slot, len(pence))
	for i, p := range pence {
		from := start.Add(time.Duration(i) * SlotLength)
		to := from.Add(SlotLength)
		slots[i] = Slot{
			TariffCode: "E-1R-AGILE-24-10-01-A",
			ValidFrom:  from,
			ValidTo:    &to,
			// exc is only carried along; every derivation here is on inc.
			ExcVATPence: p / 1.05,
			IncVATPence: p,
		}
	}
	return Curve{
		From:  start,
		To:    start.Add(time.Duration(len(pence)) * SlotLength),
		Slots: slots,
	}
}

func TestClassifyAgreesWithThePerSlotMethods(t *testing.T) {
	cases := []struct {
		name  string
		curve Curve
	}{
		{"empty", Curve{}},
		{"single slot", curveFrom(24.5)},
		{"flat day, every price identical", curveFrom(20, 20, 20, 20, 20, 20)},
		{"ordinary spread", curveFrom(12.5, 18.25, 24.5, 31.75, 42.0, 9.5)},
		{"ties at the cheapest price", curveFrom(10, 10, 10, 25, 40)},
		{"ties at the dearest price", curveFrom(10, 25, 40, 40, 40)},
		{"all plunge, median at or below zero", curveFrom(-5, -3, -1, 0)},
		{"mixed signs either side of zero", curveFrom(-4.2, 0, 3.5, 28.4, 61.1)},
		{"exactly on the band thresholds", curveFrom(
			// median is 20; the thresholds are 17 and 23 exactly.
			17, 20, 20, 23,
		)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.curve.Classify()
			priced := tc.curve.Priced()

			if len(got) != len(priced) {
				t.Fatalf("Classify returned %d entries for %d priced slots", len(got), len(priced))
			}

			for i, sl := range priced {
				if !got[i].Slot.ValidFrom.Equal(sl.ValidFrom) {
					t.Errorf("entry %d is slot %s, want %s (Classify must keep Priced order)",
						i, got[i].Slot.ValidFrom.Format(time.RFC3339), sl.ValidFrom.Format(time.RFC3339))
				}
				if want := tc.curve.BandOf(sl); got[i].Band != want {
					t.Errorf("slot %d (%.2fp): band %q, want %q", i, sl.IncVATPence, got[i].Band, want)
				}
				if want := tc.curve.RankOf(sl); got[i].Rank != want {
					t.Errorf("slot %d (%.2fp): rank %d, want %d", i, sl.IncVATPence, got[i].Rank, want)
				}
				if want := tc.curve.PercentileOf(sl); math.Abs(got[i].Percentile-want) > 1e-12 {
					t.Errorf("slot %d (%.2fp): percentile %v, want %v", i, sl.IncVATPence, got[i].Percentile, want)
				}
			}
		})
	}
}

// The real recorded day, through both paths. A synthetic curve can miss what a
// published day does — ten negative slots pulling the mean below the median is the
// case the banding thresholds were chosen against.
func TestClassifyAgreesOnTheRealRecordedDay(t *testing.T) {
	slots := fixtureSlots(t, "unit_rates_mixed_sign_day.json")
	c := Curve{
		From:  slots[0].ValidFrom,
		To:    slots[len(slots)-1].ValidFrom.Add(SlotLength),
		Slots: slots,
	}

	got := c.Classify()
	for i, sl := range c.Priced() {
		if want := c.BandOf(sl); got[i].Band != want {
			t.Errorf("slot %s: band %q, want %q", sl.ValidFrom.Format(time.RFC3339), got[i].Band, want)
		}
		if want := c.RankOf(sl); got[i].Rank != want {
			t.Errorf("slot %s: rank %d, want %d", sl.ValidFrom.Format(time.RFC3339), got[i].Rank, want)
		}
	}

	// This day is a better tie test than anything synthetic: it holds 44 distinct
	// prices across 48 slots, because four slots sit together at −2.625p (a plunge
	// plateau) and two more at 26.649p. So the invariant is not "all ranks distinct"
	// — it is that equal prices share a rank and unequal ones do not, which is what
	// a rank counting <= instead of < would break.
	ranks := map[float64]int{}
	for _, e := range got {
		if prev, seen := ranks[e.Slot.IncVATPence]; seen && prev != e.Rank {
			t.Errorf("price %.4fp got ranks %d and %d; tied prices must share a rank",
				e.Slot.IncVATPence, prev, e.Rank)
		}
		ranks[e.Slot.IncVATPence] = e.Rank
	}

	distinctRanks := map[int]bool{}
	for _, r := range ranks {
		distinctRanks[r] = true
	}
	if len(distinctRanks) != len(ranks) {
		t.Errorf("%d distinct prices produced %d distinct ranks; unequal prices must rank differently",
			len(ranks), len(distinctRanks))
	}
	if len(ranks) != 44 {
		t.Errorf("fixture has %d distinct prices, want 44 — the tie coverage this test "+
			"relies on has changed", len(ranks))
	}
}

// ---------------------------------------------------------------------------
// Benchmarks: what a /prices render costs per slot, at the window sizes the route
// actually accepts.
//
// 17,520 is a year, which `window=custom` allows today with no cap.
// ---------------------------------------------------------------------------

func benchCurve(n int) Curve {
	pence := make([]float64, n)
	for i := range pence {
		// A plausible daily shape, so the median and the bands are meaningful.
		pence[i] = 25 + 15*math.Sin(float64(i)/24*math.Pi)
	}
	return curveFrom(pence...)
}

func BenchmarkRenderPerSlotMethods(b *testing.B) {
	for _, n := range []int{48, 1488, 17520} {
		b.Run(fmt.Sprintf("slots=%d", n), func(b *testing.B) {
			c := benchCurve(n)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				for _, s := range c.Slots {
					_ = c.BandOf(s)
					_ = c.RankOf(s)
					_ = c.PercentileOf(s)
				}
			}
		})
	}
}

func BenchmarkRenderClassify(b *testing.B) {
	for _, n := range []int{48, 1488, 17520} {
		b.Run(fmt.Sprintf("slots=%d", n), func(b *testing.B) {
			c := benchCurve(n)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_ = c.Classify()
			}
		})
	}
}
