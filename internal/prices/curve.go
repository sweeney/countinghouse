package prices

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// Curve is a window of the price archive, with the derivations a consumer needs to
// act on it.
//
// The archive answers "what did a kWh cost". The curve answers "when is it cheap",
// which is the only question that can actually move a kWh to a different hour — so
// this is where the ranking, banding and run-finding live, computed once here
// rather than reinvented by every dashboard. Two consumers disagreeing about what
// "cheap" means is how a house ends up with two screens contradicting each other.
//
// Money in this type is PENCE, as the archive stores it. The summary and band
// helpers report VAT-INCLUSIVE pence, because that is what a consumer is actually
// charged; the raw slot fields keep both forms.
type Curve struct {
	// From and To bound the window that was asked for, which is what makes a GAP
	// detectable: without them a short answer is indistinguishable from a short
	// question.
	From, To time.Time

	// Slots are the prices held, oldest first. Sparse: a window may have holes.
	Slots []Slot

	// Derived once by NewCurve, because computing them per slot made rendering
	// quadratic — 2.98s of CPU for a year-long window. All three are read-only after
	// construction, so copying a Curve by value (which the Pricer interface requires)
	// shares them rather than duplicating the work.
	//
	// Absent on a Curve built as a literal. median, RankOf and PercentileOf then fall
	// back to the linear path, so for those this is an optimisation rather than a
	// precondition.
	//
	// RateAt is the exception and it matters: once byStart is built, a miss returns
	// "unknown" instead of scanning. That assumes every slot starts on the half-hour
	// grid and lasts one slot — true of everything NewCurve is given today, because
	// curveFor is only called for half-hourly segments and Gate A refuses unaligned
	// starts and off-length durations. A longer row reaching a Curve this way would
	// price as unknown rather than being found by Covers.
	sortedInc []float64     // every slot's inc-VAT pence, ascending
	byStart   map[int64]int // slot start (unix seconds) → index into Slots
	med       float64
	hasMed    bool
}

// SlotLength is the half-hour the archive is keyed on.
const SlotLength = 30 * time.Minute

// Band classifies a slot against the rest of its window.
type Band string

const (
	// BandPlunge is a price at or below zero — free energy, or being paid to take
	// it. Deliberately its own band rather than folded into "cheap": it is
	// categorically different, and it is the signal most worth surfacing.
	BandPlunge Band = "plunge"
	BandCheap  Band = "cheap"
	BandNormal Band = "normal"
	BandPeak   Band = "peak"
)

// bandThreshold is how far from the window's MEDIAN a price must sit to be called
// cheap or peak, as a fraction of the median.
//
// Two choices here, both learned the hard way.
//
// Relative to a centre rather than by percentile: a percentile (or tercile) split
// ALWAYS labels a fixed fraction of the window as a peak, which on a flat day is
// simply false and tells a consumer to avoid an hour costing the same as every
// other. A centre-relative band respects spread, so a flat day is entirely normal.
//
// And the MEDIAN, not the mean. Measured against a real published day: 48 slots
// with ten at negative prices pulled the mean down to 24.48p, which put 25 of the
// 48 above a mean-relative peak threshold. Labelling half a day "peak" dilutes the
// signal to nothing. A cluster of plunge slots is exactly the outlier a median
// shrugs off, and plunge days are precisely the days this endpoint exists to
// surface — so the statistic must survive them.
const bandThreshold = 0.15

// Summary is the window's headline figures, in VAT-INCLUSIVE pence.
type Summary struct {
	Slots int
	Min   float64
	Max   float64
	Mean  float64

	// Median is the centre BandOf measures against, and it is reported for exactly
	// that reason: a consumer is invited to derive its own banding from the summary
	// if it dislikes ours, which it cannot do without the figure the bands are
	// actually drawn from. Mean is the obvious candidate and the wrong one — a
	// cluster of plunge slots drags it well below the middle of the day.
	Median float64
}

// Priced returns the slots that carry a price, oldest first.
func (c Curve) Priced() []Slot {
	out := make([]Slot, len(c.Slots))
	copy(out, c.Slots)
	sort.SliceStable(out, func(i, j int) bool { return out[i].ValidFrom.Before(out[j].ValidFrom) })
	return out
}

// Summary computes the window's headline figures.
//
// An empty curve yields zeroes rather than NaN. A mean of nothing is undefined,
// but NaN would serialise as null or break a chart axis, and Slots==0 already says
// the window is empty to anything that looks.
func (c Curve) Summary() Summary {
	if len(c.Slots) == 0 {
		return Summary{}
	}
	s := Summary{Slots: len(c.Slots), Min: c.Slots[0].IncVATPence, Max: c.Slots[0].IncVATPence}
	var total float64
	for _, sl := range c.Slots {
		if sl.IncVATPence < s.Min {
			s.Min = sl.IncVATPence
		}
		if sl.IncVATPence > s.Max {
			s.Max = sl.IncVATPence
		}
		total += sl.IncVATPence
	}
	s.Mean = total / float64(len(c.Slots))
	s.Median = c.median()
	return s
}

// Median exposes the banding centre to callers outside this package.
func (c Curve) Median() float64 { return c.median() }

// BandOf classifies one slot against its window.
func (c Curve) BandOf(s Slot) Band {
	// Checked first and absolutely, not relatively: free energy is free whatever
	// the rest of the day costs.
	if s.IncVATPence <= 0 {
		return BandPlunge
	}
	mid := c.median()
	if mid <= 0 {
		// A median at or below zero means most of the window is free or paid. There
		// is no meaningful "expensive" to contrast against, so everything priced
		// above zero is simply normal.
		return BandNormal
	}
	switch {
	case s.IncVATPence <= mid*(1-bandThreshold):
		return BandCheap
	case s.IncVATPence >= mid*(1+bandThreshold):
		return BandPeak
	default:
		return BandNormal
	}
}

// median returns the middle VAT-inclusive price of the window, or 0 when empty.
//
// The centre for banding. Robust to the plunge clusters that make a mean
// unrepresentative — see bandThreshold.
func (c Curve) median() float64 {
	if c.hasMed {
		return c.med
	}
	return c.medianSlow()
}

func (c Curve) medianSlow() float64 {
	if len(c.Slots) == 0 {
		return 0
	}
	vals := make([]float64, 0, len(c.Slots))
	for _, s := range c.Slots {
		vals = append(vals, s.IncVATPence)
	}
	sort.Float64s(vals)
	n := len(vals)
	if n%2 == 1 {
		return vals[n/2]
	}
	return (vals[n/2-1] + vals[n/2]) / 2
}

// RankOf returns the slot's position by price, 1 being the CHEAPEST.
//
// Cheapest-first because the question being asked is "when should I run this",
// so rank 1 should be the answer rather than the thing to avoid.
func (c Curve) RankOf(s Slot) int {
	if c.sortedInc != nil {
		// Binary search on the ascending prices: the rank is how many are strictly
		// cheaper, plus one. Was a full scan per slot, which is what made rendering
		// quadratic when called for every slot in the window.
		return sort.SearchFloat64s(c.sortedInc, s.IncVATPence) + 1
	}
	return c.rankOfSlow(s)
}

func (c Curve) rankOfSlow(s Slot) int {
	rank := 1
	for _, other := range c.Slots {
		if other.IncVATPence < s.IncVATPence {
			rank++
		}
	}
	return rank
}

// PercentileOf returns the slot's position in [0,1], 0 being the cheapest.
//
// Served alongside the band so a consumer that dislikes our thresholds can derive
// its own without refetching, and so two dashboards can at least agree on the
// underlying ordering.
func (c Curve) PercentileOf(s Slot) float64 {
	if len(c.Slots) <= 1 {
		return 0
	}
	return float64(c.RankOf(s)-1) / float64(len(c.Slots)-1)
}

// Run is a contiguous stretch of slots, and what it would cost on average.
type Run struct {
	From, To        time.Time
	Slots           int
	MeanExcVATPence float64
	MeanIncVATPence float64
}

// CheapestRun finds the contiguous stretch of at least d with the lowest mean
// price, optionally required to FINISH by notAfter.
//
// Three decisions worth stating:
//
// It will not span a GAP. Slots either side of a missing price are not adjacent in
// any useful sense — you cannot schedule a load across half hours whose price we do
// not hold, and quoting such a window as cheap would be a guess wearing the shape
// of an answer.
//
// d rounds UP to whole slots. You cannot buy a third of a slot at that slot's
// price, and rounding down would quote a window too short to finish the job.
//
// notAfter is a deadline for FINISHING, not for starting. A load that overruns into
// unpriced or expensive time was not scheduled, it was merely begun.
func (c Curve) CheapestRun(d time.Duration, notAfter time.Time) (Run, bool) {
	if d <= 0 || len(c.Slots) == 0 {
		return Run{}, false
	}
	need := int((d + SlotLength - 1) / SlotLength) // ceil
	if need == 0 {
		need = 1
	}

	var best Run
	found := false
	for _, group := range c.contiguousGroups() {
		if len(group) < need {
			continue
		}
		// Prefix sums keep this linear per group rather than quadratic.
		excSum := make([]float64, len(group)+1)
		incSum := make([]float64, len(group)+1)
		for i, s := range group {
			excSum[i+1] = excSum[i] + s.ExcVATPence
			incSum[i+1] = incSum[i] + s.IncVATPence
		}
		for i := 0; i+need <= len(group); i++ {
			end := *group[i+need-1].ValidTo
			if !notAfter.IsZero() && end.After(notAfter) {
				continue
			}
			meanExc := (excSum[i+need] - excSum[i]) / float64(need)
			if found && meanExc >= best.MeanExcVATPence {
				continue
			}
			best = Run{
				From:            group[i].ValidFrom,
				To:              end,
				Slots:           need,
				MeanExcVATPence: meanExc,
				MeanIncVATPence: (incSum[i+need] - incSum[i]) / float64(need),
			}
			found = true
		}
	}
	return best, found
}

// contiguousGroups splits the curve into runs of slots that actually abut, so
// nothing downstream can treat a gap as adjacency.
func (c Curve) contiguousGroups() [][]Slot {
	priced := c.Priced()
	var groups [][]Slot
	var cur []Slot
	for _, s := range priced {
		if s.ValidTo == nil {
			// Open-ended slots are standing charges, not curve points; they have no
			// place in a run.
			continue
		}
		if len(cur) > 0 {
			prev := cur[len(cur)-1]
			if prev.ValidTo == nil || !prev.ValidTo.Equal(s.ValidFrom) {
				groups = append(groups, cur)
				cur = nil
			}
		}
		cur = append(cur, s)
	}
	if len(cur) > 0 {
		groups = append(groups, cur)
	}
	return groups
}

// Missing returns the half-hour boundaries in [From, To) that hold no price.
//
// Reported explicitly because a short answer to a long question is otherwise
// indistinguishable from a complete one. An endpoint that returned 46 slots for a
// 48-slot window and left the consumer to notice would be the silent-gap failure
// in a new place.
func (c Curve) Missing() []time.Time {
	if c.From.IsZero() || c.To.IsZero() {
		return nil
	}
	have := make(map[time.Time]bool, len(c.Slots))
	for _, s := range c.Slots {
		have[s.ValidFrom.UTC()] = true
	}
	var out []time.Time
	for t := c.From.UTC(); t.Before(c.To); t = t.Add(SlotLength) {
		if !have[t] {
			out = append(out, t)
		}
	}
	return out
}

// Complete reports whether every half hour of the window holds a price.
func (c Curve) Complete() bool { return len(c.Missing()) == 0 }

// DayStats is one local day's aggregates.
type DayStats struct {
	// Day is the LOCAL calendar date, which is how a person thinks about "a day"
	// and the only framing in which a 23- or 25-hour day makes sense.
	Day string

	Slots int

	MinExcVATPence  float64
	MaxExcVATPence  float64
	MeanExcVATPence float64

	// SpreadExcVATPence is max − min: the single number that says whether shifting
	// load that day was worth the bother.
	SpreadExcVATPence float64

	// The same four figures INC VAT.
	//
	// Both bases are carried because this struct mixed them and the mix was a trap:
	// min/max/mean/spread were ex-VAT while PlungeSlots counted on the inc-VAT value,
	// and the sibling curve endpoints are inc-VAT throughout. That difference was
	// documented in three places, which is three places saying a thing that would still
	// surprise whoever compared a spread here against a price on /prices. Emitting both
	// costs nothing and removes the trap instead of describing it.
	MinIncVATPence    float64
	MaxIncVATPence    float64
	MeanIncVATPence   float64
	SpreadIncVATPence float64

	// PlungeSlots counts half hours priced at or below zero.
	PlungeSlots int
}

// DailyStats groups the curve by local calendar day, oldest first.
func (c Curve) DailyStats(loc *time.Location) []DayStats {
	if loc == nil {
		loc = time.UTC
	}
	byDay := map[string][]Slot{}
	for _, s := range c.Slots {
		key := s.ValidFrom.In(loc).Format("2006-01-02")
		byDay[key] = append(byDay[key], s)
	}
	days := make([]string, 0, len(byDay))
	for d := range byDay {
		days = append(days, d)
	}
	sort.Strings(days)

	out := make([]DayStats, 0, len(days))
	for _, d := range days {
		slots := byDay[d]
		st := DayStats{
			Day: d, Slots: len(slots),
			MinExcVATPence: slots[0].ExcVATPence, MaxExcVATPence: slots[0].ExcVATPence,
			MinIncVATPence: slots[0].IncVATPence, MaxIncVATPence: slots[0].IncVATPence,
		}
		var totalExc, totalInc float64
		for _, s := range slots {
			if s.ExcVATPence < st.MinExcVATPence {
				st.MinExcVATPence = s.ExcVATPence
			}
			if s.ExcVATPence > st.MaxExcVATPence {
				st.MaxExcVATPence = s.ExcVATPence
			}
			// Tracked independently rather than grossed up from the ex-VAT extremes: VAT
			// on a NEGATIVE price makes it more negative, so the cheapest slot ex-VAT is
			// the cheapest inc-VAT — but assuming a fixed multiplier would bake in a VAT
			// rate this package deliberately does not hold.
			if s.IncVATPence < st.MinIncVATPence {
				st.MinIncVATPence = s.IncVATPence
			}
			if s.IncVATPence > st.MaxIncVATPence {
				st.MaxIncVATPence = s.IncVATPence
			}
			if s.IncVATPence <= 0 {
				st.PlungeSlots++
			}
			totalExc += s.ExcVATPence
			totalInc += s.IncVATPence
		}
		st.MeanExcVATPence = totalExc / float64(len(slots))
		st.SpreadExcVATPence = st.MaxExcVATPence - st.MinExcVATPence
		st.MeanIncVATPence = totalInc / float64(len(slots))
		st.SpreadIncVATPence = st.MaxIncVATPence - st.MinIncVATPence
		out = append(out, st)
	}
	return out
}

// NewCurve builds a curve over [from, to) from slots, precomputing the derivations that
// would otherwise be recomputed per slot.
//
// Rendering used to be QUADRATIC. RankOf is O(n) in the window, PercentileOf calls it,
// and BandOf re-sorted every price to find the median — once per slot. Measured on this
// code before the change: 48 slots 0.1ms, a month 44ms, a YEAR 2.98s of CPU for one
// request, with ~2 MB of response behind it. RateAt was separately a linear scan per
// lookup, which cost 883ms to price a year for ONE device, times the fleet.
//
// The fix needs no API change: sort the prices once, index the slots by start once, take
// the median once. docs/octopus-price-data-model.md §5 specified the slot index in the
// first place and the implementation kept a slice.
//
// A Curve built as a literal still works — every accessor falls back to the linear path
// when the derived fields are absent — so this is an optimisation rather than a
// precondition. Serving paths go through here.
func NewCurve(from, to time.Time, slots []Slot) Curve {
	c := Curve{From: from, To: to, Slots: slots}

	// Ambiguity is detected inside the index build rather than in a pass of its own:
	// a separate pass meant a second map the size of byStart, which cost ~1.9x the
	// construction time and twice the memory on a path that runs for every /prices
	// request — to find something that does not occur while the house is on a tariff
	// publishing one payment method. See dropAmbiguous for why they are dropped.
	c.byStart = make(map[int64]int, len(slots))
	inc := make([]float64, 0, len(slots))
	var ambiguous map[int64]bool
	for i, sl := range slots {
		k := sl.ValidFrom.UTC().Unix()
		if j, seen := c.byStart[k]; seen &&
			math.Abs(sl.IncVATPence-slots[j].IncVATPence) > priceEpsilon {
			if ambiguous == nil {
				ambiguous = make(map[int64]bool, 2)
			}
			ambiguous[k] = true
		}
		c.byStart[k] = i
		inc = append(inc, sl.IncVATPence)
	}
	if ambiguous != nil {
		// Rare enough to be worth re-entering cleanly rather than unpicking the
		// half-built index. The second pass finds nothing to drop and terminates.
		return NewCurve(from, to, dropAmbiguous(slots, ambiguous))
	}
	sort.Float64s(inc)
	c.sortedInc = inc
	if n := len(inc); n > 0 {
		if n%2 == 1 {
			c.med = inc[n/2]
		} else {
			c.med = (inc[n/2-1] + inc[n/2]) / 2
		}
		c.hasMed = true
	}
	return c
}

// Fingerprint is a string of what the curve CONTAINS: every slot's start and its
// inc-VAT price, in order.
//
// Not compact — it is the full uncompressed concatenation, roughly 52 KB for a 31-day
// window, built and discarded on every request including the ones that end in a 304.
// The caller hashes it anyway, so writing straight into the digest would avoid the
// allocation; left as a string for now because the cost is bounded by the same window
// caps that bound the response, and the clarity at the call site is worth more than the
// allocation at this size.
//
// For HTTP caching. Hashing a rendered response body is the obvious approach and is
// wrong here, because three of the four price responses carry a timestamp that moves on
// every request — so the tag changed every time and the 304 could never fire. Hashing
// only the window and tariff is the opposite error: the tag then does not move when a
// price is RESTATED, and a client serves a stale price indefinitely.
//
// This is the middle: stable across requests, and different the moment any price in the
// window changes. Prices are formatted rather than raw so the digest does not depend on
// float formatting incidentals.
func (c Curve) Fingerprint() string {
	var b strings.Builder
	for _, s := range c.Slots {
		fmt.Fprintf(&b, "%s=%.6f;", s.ValidFrom.UTC().Format(time.RFC3339), s.IncVATPence)
	}
	return b.String()
}

// RateInterval reports the half-hour slot grid, satisfying energy.Granularity.
//
// This is what tells the series layer that a bucket coarser than a half hour cannot
// be priced at the rate holding at its start — the difference between a monthly
// chart's cost agreeing with the monthly bill and being out by tens of percent.
func (c Curve) RateInterval() time.Duration { return SlotLength }

// RateAt returns the VAT-inclusive £/kWh for the slot covering t, satisfying
// energy.Pricer.
//
// £ rather than the pence the archive stores, because the cost layer works in
// pounds; and VAT-inclusive because that is what is actually charged.
//
// The supplier's own inc-VAT figure is used rather than one computed from ex-VAT.
// It carries more precision than any rounding policy we would pick, and — more to
// the point — it is the number the supplier actually bills. That is why the
// inc/exc consistency check is a Gate B WARNING rather than a Gate A rejection: if
// the two columns disagree with our configured vat_rate, the figure used here is
// unaffected, because it never passed through that rate. A slot flagged for a VAT
// mismatch is still priced from the supplier's delivered inc figure, which is the
// right answer.
//
// False means NO PRICE IS HELD for that half hour, which a caller must surface as
// unpriced energy. Returning zero would charge nothing for real consumption.
func (c Curve) RateAt(t time.Time) (float64, bool) {
	if c.byStart != nil {
		// One map lookup on the slot the instant falls in, rather than scanning the
		// window. A year-long bill was 883ms per device on the linear path.
		if i, ok := c.byStart[t.UTC().Truncate(SlotLength).Unix()]; ok {
			if sl := c.Slots[i]; sl.Covers(t) {
				return sl.IncVATPence / 100, true
			}
		}
		return 0, false
	}
	return c.rateAtSlow(t)
}

func (c Curve) rateAtSlow(t time.Time) (float64, bool) {
	for _, s := range c.Slots {
		if s.Covers(t) {
			return s.IncVATPence / 100, true
		}
	}
	return 0, false
}

// dropAmbiguous removes every slot whose interval was found to carry more than one
// price. NewCurve detects them; this drops them.
//
// The archive's key is (tariff_code, payment_method, valid_from), and it is that
// wide because a variable tariff publishes the SAME half hour once per payment
// method at different prices. Every reader here is keyed on the interval alone —
// byStart, RateAt, the rank population, Schedule.at — so two rows for one interval
// used to collapse to whichever sorted last, priced silently at it, and appeared
// twice in the slot list with rank and median computed over a duplicated
// population.
//
// Picking one is not available: which payment method the account is on is a
// configuration fact this package does not have. Refusing the rows on write is
// worse — it would make the composite key pointless, and two separate Put calls
// could reach the same state anyway.
//
// So an interval with two prices is an interval whose price we do not know, which
// Pricer's (float64, bool) already expresses. Dropping it here means RateAt misses
// it, Complete() reports the gap, and the energy in it surfaces as unpriced_kwh —
// visible, and never a confident wrong number. Identical rows are not ambiguous;
// only a genuine disagreement is.
//
// Not reachable while the house is on Agile, which publishes payment_method null.
// The archive is explicitly designed to outlive the current tariff.
func dropAmbiguous(slots []Slot, ambiguous map[int64]bool) []Slot {
	out := make([]Slot, 0, len(slots))
	for _, sl := range slots {
		if !ambiguous[sl.ValidFrom.UTC().Unix()] {
			out = append(out, sl)
		}
	}
	return out
}
