package prices

import (
	"sort"
	"time"
)

// ---------------------------------------------------------------------------
// Coarser rollups than a day (issue #36 idea 4).
//
// /prices/stats answers per DAY, which is the right grain for "was shifting load
// worth it yesterday". It is the wrong grain for "how often do cheap slots occur
// in winter versus summer" — that question needs about 24 numbers and the
// endpoint hands back about 730 rows, so every consumer asking it wrote the same
// client-side rollup.
//
// The aggregate that cannot be derived from the daily rows afterwards is the one
// worth serving: a median over a month is not the median of the daily medians,
// and "days with at least one cheap slot" cannot be recovered from daily
// min/max/mean at all.
// ---------------------------------------------------------------------------

// Period groupings for PeriodStats.
const (
	GroupByDay   = "day"
	GroupByMonth = "month"
)

// ValidPeriodGrouping reports whether g is an accepted grouping.
func ValidPeriodGrouping(g string) bool { return g == GroupByDay || g == GroupByMonth }

// PeriodStats is one rollup bucket: a day or a month of half-hourly prices.
//
// Prices are pence. The unsuffixed figures are INC VAT, matching every sibling
// price route; the _exc_vat siblings carry the analytical basis, since ex-VAT is
// what the supplier publishes and what comparing two periods wants.
type PeriodStats struct {
	// Period is "2026-06" for a month, "2026-06-14" for a day.
	Period string `json:"period"`

	// Days is how many local calendar days the bucket actually holds prices for —
	// NOT the length of the month. A partial month at either end of the window
	// would otherwise look like a full one.
	Days  int `json:"days"`
	Slots int `json:"slots"`

	Min    float64 `json:"min"`
	Max    float64 `json:"max"`
	Mean   float64 `json:"mean"`
	Median float64 `json:"median"`

	MinExcVAT    float64 `json:"min_exc_vat"`
	MaxExcVAT    float64 `json:"max_exc_vat"`
	MeanExcVAT   float64 `json:"mean_exc_vat"`
	MedianExcVAT float64 `json:"median_exc_vat"`

	// MeanSpread is the mean of the DAILY spreads in the bucket, not
	// max − min over the whole period. The daily spread is the number that says
	// whether shifting load that day was worth the bother; averaging it answers
	// "how worthwhile was shifting load in this month". A period-wide max − min
	// answers something else entirely — it is dominated by the single most extreme
	// day and says nothing about a typical one.
	MeanSpread float64 `json:"mean_spread"`

	// PlungeSlots counts half hours at or below zero; NegativeSlots counts those
	// strictly below. They differ by the exactly-zero slots, which are free but
	// not paid-to-take, and a consumer chasing export revenue wants the latter.
	PlungeSlots   int `json:"plunge_slots"`
	NegativeSlots int `json:"negative_slots"`

	// CheapSlots and CheapDays are populated only when the caller supplies a
	// threshold, because "cheap" is a policy rather than a fact — the same
	// argument the floorplan `category` passthrough already makes.
	//
	// CheapDays is the one that cannot be derived from daily rows afterwards, and
	// is usually the one actually wanted: "how many days in January could I have
	// run the dishwasher under 10p" is a different question from "how many half
	// hours were under 10p", and the second can be one freak night.
	CheapSlots *int `json:"cheap_slots,omitempty"`
	CheapDays  *int `json:"cheap_days,omitempty"`
}

// PeriodStatsOver groups the curve into buckets of the requested grouping,
// oldest first.
//
// cheapBelow is an optional inc-VAT pence threshold; nil leaves the cheap counts
// off rather than picking a definition of cheap on the caller's behalf.
func (c Curve) PeriodStatsOver(loc *time.Location, groupBy string, cheapBelow *float64) []PeriodStats {
	if loc == nil {
		loc = time.UTC
	}
	daily := c.DailyStats(loc)
	if len(daily) == 0 {
		return nil
	}

	// Slots are re-grouped from the curve rather than reconstructed from the daily
	// rows: a median and a mean need every value, and the daily rows hold only
	// summaries. Keyed the same way DailyStats keys them so the two cannot drift.
	byPeriod := map[string][]Slot{}
	for _, s := range c.Slots {
		byPeriod[periodKey(s.ValidFrom.In(loc), groupBy)] = append(byPeriod[periodKey(s.ValidFrom.In(loc), groupBy)], s)
	}

	// Daily summaries per period, for MeanSpread and Days.
	daysIn := map[string][]DayStats{}
	for _, d := range daily {
		if t, err := time.ParseInLocation("2006-01-02", d.Day, loc); err == nil {
			daysIn[periodKey(t, groupBy)] = append(daysIn[periodKey(t, groupBy)], d)
		}
	}

	keys := make([]string, 0, len(byPeriod))
	for k := range byPeriod {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	out := make([]PeriodStats, 0, len(keys))
	for _, k := range keys {
		out = append(out, summarise(k, byPeriod[k], daysIn[k], cheapBelow, loc, groupBy))
	}
	return out
}

// periodKey is the bucket a local instant falls in.
func periodKey(localT time.Time, groupBy string) string {
	if groupBy == GroupByMonth {
		return localT.Format("2006-01")
	}
	return localT.Format("2006-01-02")
}

func summarise(key string, slots []Slot, days []DayStats, cheapBelow *float64, loc *time.Location, groupBy string) PeriodStats {
	st := PeriodStats{
		Period: key,
		Days:   len(days),
		Slots:  len(slots),
		Min:    slots[0].IncVATPence, Max: slots[0].IncVATPence,
		MinExcVAT: slots[0].ExcVATPence, MaxExcVAT: slots[0].ExcVATPence,
	}

	inc := make([]float64, 0, len(slots))
	exc := make([]float64, 0, len(slots))
	var totalInc, totalExc float64
	cheapByDay := map[string]bool{}
	cheapSlots := 0

	for _, s := range slots {
		if s.IncVATPence < st.Min {
			st.Min = s.IncVATPence
		}
		if s.IncVATPence > st.Max {
			st.Max = s.IncVATPence
		}
		if s.ExcVATPence < st.MinExcVAT {
			st.MinExcVAT = s.ExcVATPence
		}
		if s.ExcVATPence > st.MaxExcVAT {
			st.MaxExcVAT = s.ExcVATPence
		}
		if s.IncVATPence <= 0 {
			st.PlungeSlots++
		}
		if s.IncVATPence < 0 {
			st.NegativeSlots++
		}
		if cheapBelow != nil && s.IncVATPence <= *cheapBelow {
			cheapSlots++
			cheapByDay[s.ValidFrom.In(loc).Format("2006-01-02")] = true
		}
		inc = append(inc, s.IncVATPence)
		exc = append(exc, s.ExcVATPence)
		totalInc += s.IncVATPence
		totalExc += s.ExcVATPence
	}

	n := float64(len(slots))
	st.Mean, st.MeanExcVAT = totalInc/n, totalExc/n
	st.Median, st.MedianExcVAT = median(inc), median(exc)

	if len(days) > 0 {
		var spread float64
		for _, d := range days {
			spread += d.SpreadIncVATPence
		}
		st.MeanSpread = spread / float64(len(days))
	}
	// A day grouping holds exactly one day by construction; reporting 0 because
	// the daily join missed would be a silent inconsistency with Slots.
	if groupBy == GroupByDay && st.Days == 0 {
		st.Days = 1
	}

	if cheapBelow != nil {
		cs, cd := cheapSlots, len(cheapByDay)
		st.CheapSlots, st.CheapDays = &cs, &cd
	}
	return st
}

// median of a slice, which it sorts a copy of. Even counts take the mean of the
// two middle values.
func median(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := make([]float64, len(v))
	copy(s, v)
	sort.Float64s(s)
	mid := len(s) / 2
	if len(s)%2 == 1 {
		return s[mid]
	}
	return (s[mid-1] + s[mid]) / 2
}
