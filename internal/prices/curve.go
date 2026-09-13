package prices

import (
	"sort"
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

	// idx accelerates RateAt, and is nil unless the curve was built by NewCurve
	// AND its slots are well formed enough for a lookup to be provably identical
	// to the scan — see newRateIndex. A nil idx is not a correctness problem, only
	// a slower one, so a hand-built Curve{...} literal still answers correctly.
	idx *rateIndex
}

// rateIndex is the lookup structure behind RateAt.
//
// Slots ascending by start, with their starts lifted out as UnixNano so the search
// compares integers rather than calling time.Time.Before through an interface. Only
// ever built for a curve whose slots are disjoint, which is what makes a binary
// search equivalent to the scan it replaces.
type rateIndex struct {
	starts []int64
	slots  []Slot
}

// NewCurve builds a curve over [from, to) and indexes its slots for lookup.
//
// The index is what keeps the cost path linear rather than quadratic: pricing a
// month bills 1,488 half-hourly buckets PER DEVICE, and each one is a RateAt. With
// a scan that is 12.7ms a device over a month and 1.55s over a year; with the index
// it is a binary search.
//
// Prefer this to a Curve literal anywhere the curve will be priced against. A
// literal is not wrong — RateAt falls back to the scan — it is just slow, which is
// why the indexed path is the one the constructor gives you by default.
func NewCurve(from, to time.Time, slots []Slot) Curve {
	return Curve{From: from, To: to, Slots: slots, idx: newRateIndex(slots)}
}

// newRateIndex indexes slots, or returns nil when it cannot do so SAFELY.
//
// RateAt's contract is the first slot in Slots order that covers the instant. A
// binary search can only reproduce that when at most one slot covers any instant,
// so anything that could make two slots cover one instant refuses the index and
// keeps the scan:
//
//   - an open-ended slot (a standing charge, or a flat rate not yet superseded),
//     which by definition overlaps everything after it;
//   - two slots sharing a start, which is what a variable tariff's DIRECT_DEBIT and
//     NON_DIRECT_DEBIT rows are — the same half hour at two prices;
//   - any other overlap, which Gate C reports as two prices for one kWh.
//
// Refusing rather than approximating matters here more than the speed does: this
// function decides what a kWh cost.
func newRateIndex(slots []Slot) *rateIndex {
	if len(slots) == 0 {
		return nil
	}

	sorted := make([]Slot, len(slots))
	copy(sorted, slots)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].ValidFrom.Before(sorted[j].ValidFrom) })

	starts := make([]int64, len(sorted))
	for i, s := range sorted {
		if s.ValidTo == nil {
			return nil // unbounded: overlaps everything after it
		}
		if i > 0 {
			prev := sorted[i-1]
			if s.ValidFrom.Equal(prev.ValidFrom) {
				return nil // two prices for one half hour
			}
			if prev.ValidTo.After(s.ValidFrom) {
				return nil // the previous slot has not ended when this one starts
			}
		}
		starts[i] = s.ValidFrom.UnixNano()
	}
	return &rateIndex{starts: starts, slots: sorted}
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
	return s
}

// BandOf classifies one slot against its window.
//
// Rendering a whole window calls Classify instead, which computes this for every
// slot in one pass. Both go through bandFor, so they cannot disagree.
func (c Curve) BandOf(s Slot) Band { return bandFor(s.IncVATPence, c.median()) }

// bandFor is the banding rule itself, given a price and the window's median.
//
// Split out so the per-slot method and the whole-window pass share one definition
// of "cheap" — the same reason the bands are served at all rather than left to each
// consumer.
func bandFor(incVATPence, mid float64) Band {
	// Checked first and absolutely, not relatively: free energy is free whatever
	// the rest of the day costs.
	if incVATPence <= 0 {
		return BandPlunge
	}
	if mid <= 0 {
		// A median at or below zero means most of the window is free or paid. There
		// is no meaningful "expensive" to contrast against, so everything priced
		// above zero is simply normal.
		return BandNormal
	}
	switch {
	case incVATPence <= mid*(1-bandThreshold):
		return BandCheap
	case incVATPence >= mid*(1+bandThreshold):
		return BandPeak
	default:
		return BandNormal
	}
}

// median returns the middle VAT-inclusive price of the window, or 0 when empty.
//
// The centre for banding. Robust to the plunge clusters that make a mean
// unrepresentative — see bandThreshold.
func (c Curve) median() float64 { return medianOf(c.sortedPrices()) }

// sortedPrices returns the window's inc-VAT prices, ascending.
//
// The one array both the median and the ranking need. Building it once is what
// turns rendering a window from quadratic into one sort.
func (c Curve) sortedPrices() []float64 {
	vals := make([]float64, 0, len(c.Slots))
	for _, s := range c.Slots {
		vals = append(vals, s.IncVATPence)
	}
	sort.Float64s(vals)
	return vals
}

// medianOf returns the middle value of an ASCENDING slice, or 0 when empty.
func medianOf(sorted []float64) float64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	if n%2 == 1 {
		return sorted[n/2]
	}
	return (sorted[n/2-1] + sorted[n/2]) / 2
}

// rankIn returns a price's position in an ASCENDING slice, 1 being the cheapest.
//
// The number of prices strictly below it, plus one — identical to what RankOf
// counts, so tied prices share the better rank in both. A binary search rather
// than a scan, which is the whole point: the scan made rendering a window
// quadratic in its own length.
func rankIn(sorted []float64, price float64) int {
	return sort.SearchFloat64s(sorted, price) + 1
}

// percentileFor maps a rank in a window of n prices to [0,1], 0 being cheapest.
func percentileFor(rank, n int) float64 {
	if n <= 1 {
		return 0
	}
	return float64(rank-1) / float64(n-1)
}

// RankOf returns the slot's position by price, 1 being the CHEAPEST.
//
// Cheapest-first because the question being asked is "when should I run this",
// so rank 1 should be the answer rather than the thing to avoid.
// A count rather than a binary search, deliberately: sorting to place ONE price
// costs more than scanning for it (measured: sorting here made a year-long window
// six times slower). Classify sorts once for the whole window and uses rankIn
// instead; the two are pinned equal by TestClassifyAgreesWithThePerSlotMethods.
func (c Curve) RankOf(s Slot) int {
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
	return percentileFor(c.RankOf(s), len(c.Slots))
}

// SlotClass is one slot with the derivations a consumer would otherwise compute
// itself — and would compute differently from the next consumer.
type SlotClass struct {
	Slot       Slot
	Band       Band
	Rank       int
	Percentile float64
}

// Classify returns every slot in the window with its band, rank and percentile,
// oldest first — the same answers BandOf, RankOf and PercentileOf give, computed
// in ONE pass over the window rather than one pass per slot.
//
// This is what a /prices render should call. Asking the per-slot methods in a loop
// sorts the window once per slot for the median and scans it once per slot for the
// rank, which is quadratic in the number of slots: measured at 57ms for a month and
// 10.3s for a year, against a window the route accepts today with no cap. Here it
// is one sort and a binary search per slot.
//
// The methods are kept, and now delegate to the same helpers, so a caller holding
// one slot need not classify a whole window and the two can never disagree.
func (c Curve) Classify() []SlotClass {
	priced := c.Priced()
	if len(priced) == 0 {
		return nil
	}

	sorted := c.sortedPrices()
	mid := medianOf(sorted)
	n := len(c.Slots)

	out := make([]SlotClass, 0, len(priced))
	for _, s := range priced {
		rank := rankIn(sorted, s.IncVATPence)
		out = append(out, SlotClass{
			Slot:       s,
			Band:       bandFor(s.IncVATPence, mid),
			Rank:       rank,
			Percentile: percentileFor(rank, n),
		})
	}
	return out
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
		}
		var total float64
		for _, s := range slots {
			if s.ExcVATPence < st.MinExcVATPence {
				st.MinExcVATPence = s.ExcVATPence
			}
			if s.ExcVATPence > st.MaxExcVATPence {
				st.MaxExcVATPence = s.ExcVATPence
			}
			if s.IncVATPence <= 0 {
				st.PlungeSlots++
			}
			total += s.ExcVATPence
		}
		st.MeanExcVATPence = total / float64(len(slots))
		st.SpreadExcVATPence = st.MaxExcVATPence - st.MinExcVATPence
		out = append(out, st)
	}
	return out
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
	if c.idx != nil {
		return c.idx.rateAt(t)
	}
	for _, s := range c.Slots {
		if s.Covers(t) {
			return s.IncVATPence / 100, true
		}
	}
	return 0, false
}

// rateAt is the indexed lookup: the last slot starting at or before t, which is the
// only one that can cover t once the slots are known to be disjoint.
func (r *rateIndex) rateAt(t time.Time) (float64, bool) {
	tn := t.UnixNano()
	// The first slot starting strictly after t; the one before it is the candidate.
	i := sort.Search(len(r.starts), func(i int) bool { return r.starts[i] > tn }) - 1
	if i < 0 {
		return 0, false // t precedes every slot held
	}
	// Still checked: a gap in the archive leaves the previous slot ending before t,
	// and unpriced must stay distinguishable from free.
	if s := r.slots[i]; s.Covers(t) {
		return s.IncVATPence / 100, true
	}
	return 0, false
}
