package prices

import (
	"math"
	"testing"
	"time"
)

// pdSlots builds n consecutive half-hourly slots from start at the given
// inc-VAT pence, with ex-VAT derived at 5%.
func pdSlots(start time.Time, pence ...float64) []Slot {
	out := make([]Slot, 0, len(pence))
	for i, p := range pence {
		from := start.UTC().Add(time.Duration(i) * SlotLength)
		to := from.Add(SlotLength)
		out = append(out, Slot{
			TariffCode: "T", ValidFrom: from, ValidTo: &to,
			IncVATPence: p, ExcVATPence: p / 1.05, RetrievedAt: from,
		})
	}
	return out
}

func mustUTC(t *testing.T) *time.Location { t.Helper(); return time.UTC }

// A month rollup answers a question the daily rows cannot be aggregated into
// afterwards: a median over a month is not the median of the daily medians.
func TestPeriodStatsGroupsByMonth(t *testing.T) {
	loc := mustUTC(t)
	jun := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	jul := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)

	slots := append(pdSlots(jun, 10, 20, 30, 40), pdSlots(jul, 1, 2)...)
	c := NewCurve(jun, jul.Add(24*time.Hour), slots)

	got := c.PeriodStatsOver(loc, GroupByMonth, nil)
	if len(got) != 2 {
		t.Fatalf("want 2 months, got %d: %+v", len(got), got)
	}
	if got[0].Period != "2026-06" || got[1].Period != "2026-07" {
		t.Errorf("periods = %q/%q, want 2026-06/2026-07", got[0].Period, got[1].Period)
	}
	if got[0].Slots != 4 || got[1].Slots != 2 {
		t.Errorf("slot counts = %d/%d, want 4/2", got[0].Slots, got[1].Slots)
	}
	// Median over all four slots: (20+30)/2.
	if math.Abs(got[0].Median-25) > 1e-9 {
		t.Errorf("June median = %v, want 25", got[0].Median)
	}
	if math.Abs(got[0].Mean-25) > 1e-9 {
		t.Errorf("June mean = %v, want 25", got[0].Mean)
	}
	if got[0].Min != 10 || got[0].Max != 40 {
		t.Errorf("June min/max = %v/%v, want 10/40", got[0].Min, got[0].Max)
	}
}

// Days counts days actually holding prices, not the length of the month — a
// partial month at the edge of a window must not look like a full one.
func TestPeriodStatsCountsOnlyDaysHoldingPrices(t *testing.T) {
	loc := mustUTC(t)
	start := time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC)
	// Two slots on the 10th, two on the 11th.
	slots := append(pdSlots(start, 10, 20), pdSlots(start.Add(24*time.Hour), 30, 40)...)
	c := NewCurve(start, start.Add(72*time.Hour), slots)

	got := c.PeriodStatsOver(loc, GroupByMonth, nil)
	if len(got) != 1 {
		t.Fatalf("want 1 month, got %d", len(got))
	}
	if got[0].Days != 2 {
		t.Errorf("days = %d, want 2 (not the length of June)", got[0].Days)
	}
}

// mean_spread is the mean of the DAILY spreads, not max − min over the period.
// The latter is dominated by the single most extreme day and says nothing about
// a typical one.
func TestPeriodStatsMeanSpreadAveragesDailySpreads(t *testing.T) {
	loc := mustUTC(t)
	d1 := time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC)
	d2 := d1.Add(24 * time.Hour)
	// Day 1 spread 10 (10..20), day 2 spread 30 (0..30).
	slots := append(pdSlots(d1, 10, 20), pdSlots(d2, 0, 30)...)
	c := NewCurve(d1, d2.Add(24*time.Hour), slots)

	got := c.PeriodStatsOver(loc, GroupByMonth, nil)
	if want := 20.0; math.Abs(got[0].MeanSpread-want) > 1e-9 {
		t.Errorf("mean_spread = %v, want %v (mean of 10 and 30)", got[0].MeanSpread, want)
	}
	// The period-wide max − min would be 30; that is deliberately NOT this field.
	if math.Abs(got[0].Max-got[0].Min-30) > 1e-9 {
		t.Fatal("fixture should have a period-wide spread of 30")
	}
}

// "Cheap" is a policy, so the counts appear only when the caller supplies a
// threshold.
func TestPeriodStatsCheapCountsAreOptional(t *testing.T) {
	loc := mustUTC(t)
	start := time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC)
	c := NewCurve(start, start.Add(24*time.Hour), pdSlots(start, 5, 20))

	if got := c.PeriodStatsOver(loc, GroupByMonth, nil); got[0].CheapSlots != nil || got[0].CheapDays != nil {
		t.Error("no threshold supplied, but cheap counts were invented")
	}
}

// cheap_days is the figure that cannot be recovered from daily rows: "how many
// days could I have run the dishwasher under 10p" is a different question from
// "how many half hours were under 10p", and the second can be one freak night.
func TestPeriodStatsCheapDaysIsNotCheapSlots(t *testing.T) {
	loc := mustUTC(t)
	d1 := time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC)
	d2 := d1.Add(24 * time.Hour)
	d3 := d2.Add(24 * time.Hour)
	// Day 1: four cheap slots. Day 2: none. Day 3: one cheap slot.
	slots := pdSlots(d1, 1, 2, 3, 4)
	slots = append(slots, pdSlots(d2, 50, 60)...)
	slots = append(slots, pdSlots(d3, 5, 90)...)
	c := NewCurve(d1, d3.Add(24*time.Hour), slots)

	threshold := 10.0
	got := c.PeriodStatsOver(loc, GroupByMonth, &threshold)
	if got[0].CheapSlots == nil || *got[0].CheapSlots != 5 {
		t.Errorf("cheap_slots = %v, want 5", got[0].CheapSlots)
	}
	if got[0].CheapDays == nil || *got[0].CheapDays != 2 {
		t.Errorf("cheap_days = %v, want 2 (days 1 and 3, not day 2)", got[0].CheapDays)
	}
}

// plunge is at-or-below zero; negative is strictly below. They differ by the
// exactly-zero slots, which are free but not paid-to-take.
func TestPeriodStatsSeparatesPlungeFromNegative(t *testing.T) {
	loc := mustUTC(t)
	start := time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC)
	c := NewCurve(start, start.Add(24*time.Hour), pdSlots(start, -5, 0, 10))

	got := c.PeriodStatsOver(loc, GroupByMonth, nil)
	if got[0].PlungeSlots != 2 {
		t.Errorf("plunge_slots = %d, want 2 (−5 and 0)", got[0].PlungeSlots)
	}
	if got[0].NegativeSlots != 1 {
		t.Errorf("negative_slots = %d, want 1 (only −5)", got[0].NegativeSlots)
	}
}

// A day grouping still works, and reports one day per bucket.
func TestPeriodStatsGroupsByDay(t *testing.T) {
	loc := mustUTC(t)
	d1 := time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC)
	slots := append(pdSlots(d1, 10, 20), pdSlots(d1.Add(24*time.Hour), 30, 40)...)
	c := NewCurve(d1, d1.Add(48*time.Hour), slots)

	got := c.PeriodStatsOver(loc, GroupByDay, nil)
	if len(got) != 2 {
		t.Fatalf("want 2 days, got %d", len(got))
	}
	if got[0].Period != "2026-06-10" || got[0].Days != 1 {
		t.Errorf("day bucket = %q days=%d", got[0].Period, got[0].Days)
	}
}

func TestPeriodStatsOnAnEmptyCurve(t *testing.T) {
	start := time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC)
	if got := NewCurve(start, start.Add(24*time.Hour), nil).PeriodStatsOver(time.UTC, GroupByMonth, nil); got != nil {
		t.Errorf("empty curve should yield no periods, got %+v", got)
	}
}

func TestValidPeriodGrouping(t *testing.T) {
	for _, g := range []string{GroupByDay, GroupByMonth} {
		if !ValidPeriodGrouping(g) {
			t.Errorf("ValidPeriodGrouping(%q) = false", g)
		}
	}
	for _, g := range []string{"", "week", "year", "MONTH"} {
		if ValidPeriodGrouping(g) {
			t.Errorf("ValidPeriodGrouping(%q) = true", g)
		}
	}
}
