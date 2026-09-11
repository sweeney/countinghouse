package prices

import (
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/sweeney/countinghouse/internal/octopus"
)

// Validation is three gates, and they differ in what a failure costs — which is
// why they differ in what a failure does. See docs/octopus-price-pipeline.md §2.
//
//	Gate A  structural   per slot   REJECT  — the value cannot be money
//	Gate B  plausibility per slot   FLAG    — the value is surprising, not wrong
//	Gate C  set level    per day    REPORT  — the DAY is incomplete, the rows are fine
//
// One rule sits above all three: a rejected slot is never silently dropped. It
// comes back as a Rejection carrying its reason so the caller can log it, count
// it and quarantine it. A quietly-discarded slot becomes unpriced energy weeks
// later, and by then nobody can tell whether the price was missing upstream or
// the collector ate it.

const (
	// DefaultSlotDuration is the length of an Agile interval. It is a constant
	// rather than a discovered value because a feed that starts sending some
	// other length means we have misunderstood it, and that must surface as a
	// rejection rather than be absorbed.
	DefaultSlotDuration = 30 * time.Minute

	// AnyDuration disables the length check, for the rows in this archive that
	// are not half-hourly: flat-tariff unit rates run for months at a time
	// (observed: 2026-03-31T23:00Z → 2026-06-30T23:00Z) and standing charges
	// are open-ended. Alignment is still checked; only the length is waived.
	AnyDuration = time.Duration(-1)

	// DefaultVATEpsilonPence is the tolerance on |inc − exc × (1 + vat)|.
	//
	// It has to straddle a very wide gap. Below it: float64 noise on
	// penny-scale money, which is around 1e-14. Above it: the smallest VAT
	// change anybody would legislate, which on even a 1p slot moves inc by
	// 0.0015p — a thousand times this epsilon. 1e-6p therefore cannot fire on
	// arithmetic and cannot miss a rate change or a pence/pounds unit error.
	DefaultVATEpsilonPence = 1e-6

	// DefaultMaxIncVATPence is the product's documented cap, inc VAT. Observed
	// maximum over 11 months was 76.67 exc (~80.5 inc), so the cap has real
	// headroom over anything the tariff has actually done.
	DefaultMaxIncVATPence = 100.0

	// DefaultMinIncVATPence is a sanity floor, inc VAT. No CONTRACTUAL floor
	// exists — negative prices are the point of the tariff and the observed
	// minimum was −11.11 exc — so this is set far below anything seen, purely to
	// catch a sign or scale error rather than to judge the market.
	DefaultMinIncVATPence = -50.0

	// DefaultMaxJumpPence is the adjacent-slot step that counts as a break in
	// shape, inc VAT.
	//
	// It must be generous or it fires every day and gets ignored: the median
	// intra-day SPREAD is 24.3 p/kWh, the worst observed day was 62.6, and the
	// worst observed step between two neighbouring slots across the recorded
	// fixtures is 18.1. 75 is above every one of those, so it cannot fire on a
	// shape Agile has ever produced — but a factor-of-ten scale error on a
	// normal evening price clears it easily.
	DefaultMaxJumpPence = 75.0
)

// RejectReason names the Gate A check that failed. It is a stable string
// because it is written to the quarantine file and counted as a metric label;
// renaming one breaks a dashboard and an operator's grep.
type RejectReason string

const (
	ReasonValidFromZero      RejectReason = "valid_from_zero"
	ReasonValidFromNotUTC    RejectReason = "valid_from_not_utc"
	ReasonValidFromUnaligned RejectReason = "valid_from_unaligned"
	ReasonValidToNotAfter    RejectReason = "valid_to_not_after_valid_from"
	ReasonSlotDuration       RejectReason = "slot_duration"
	ReasonPriceNotFinite     RejectReason = "price_not_finite"
	ReasonVATMismatch        RejectReason = "vat_mismatch"
	ReasonTariffCode         RejectReason = "tariff_code_unparseable"
	ReasonTariffMismatch     RejectReason = "tariff_code_mismatch"
)

// WarningKind names a Gate B finding. Also a metric label, also stable.
type WarningKind string

const (
	WarnAboveCap   WarningKind = "above_cap"
	WarnBelowFloor WarningKind = "below_floor"
	WarnJump       WarningKind = "jump"
)

// Rejection is a slot that must not be stored, with the reason why.
//
// It carries the whole Slot rather than its key: the quarantine record has to be
// replayable, and a key alone would lose the very prices that failed the check.
type Rejection struct {
	Slot   Slot
	Reason RejectReason

	// Detail is the human-readable specifics — the offending value, and what
	// was expected. A quarantined slot with no explanation is un-triageable
	// weeks later, which is when anybody actually reads the file.
	Detail string
}

// Warning is a slot that IS stored but looked surprising.
type Warning struct {
	Slot   Slot
	Kind   WarningKind
	Detail string
}

// ValidationResult is the whole verdict on one batch.
//
// Accepted is in input order, so a caller can hand it straight to Store.Put and
// reconcile it against what it fetched. Every input slot appears exactly once
// across Accepted and Rejected — that invariant is what "nothing is silently
// dropped" means in code.
type ValidationResult struct {
	Accepted []Slot
	Rejected []Rejection
	Warnings []Warning
}

// ValidateOptions configures the two per-slot gates.
//
// Every threshold defaults when left zero, so the Agile collector only has to
// state the two things that are genuinely its own: the tariff it asked for and
// the VAT rate in force.
type ValidateOptions struct {
	// TariffCode is the code we REQUESTED. When set, a slot carrying any other
	// code is rejected rather than archived under ours — the failure mode that
	// would otherwise silently bill this house on another region's prices.
	// Empty means "don't care", which is only right for a mixed batch.
	TariffCode string

	// VATRate is taken literally, with no default, because 0% VAT is a legal
	// rate and defaulting would quietly assert today's 5% over a caller that
	// meant something else. A rate change is a config change, and this check
	// has to follow config rather than contradict it.
	VATRate float64

	// SlotDuration is the exact length every bounded interval must have. Zero
	// means DefaultSlotDuration; AnyDuration waives the check.
	SlotDuration time.Duration

	// VATEpsilonPence, MaxIncVATPence, MinIncVATPence and MaxJumpPence each take
	// their documented default when zero. A floor of exactly 0 is therefore not
	// expressible — which costs nothing, because negative prices are real and a
	// floor of 0 would be wrong.
	VATEpsilonPence float64
	MaxIncVATPence  float64
	MinIncVATPence  float64
	MaxJumpPence    float64
}

func (o ValidateOptions) withDefaults() ValidateOptions {
	if o.SlotDuration == 0 {
		o.SlotDuration = DefaultSlotDuration
	}
	if o.VATEpsilonPence == 0 {
		o.VATEpsilonPence = DefaultVATEpsilonPence
	}
	if o.MaxIncVATPence == 0 {
		o.MaxIncVATPence = DefaultMaxIncVATPence
	}
	if o.MinIncVATPence == 0 {
		o.MinIncVATPence = DefaultMinIncVATPence
	}
	if o.MaxJumpPence == 0 {
		o.MaxJumpPence = DefaultMaxJumpPence
	}
	return o
}

// Validate runs Gates A and B over a batch.
//
// It takes no clock and no context: the verdict on a slot depends only on the
// slot and the options, so the same batch validates identically on a replay
// months later. That is what makes the quarantine file meaningful.
//
// Gate C is deliberately NOT here — see CheckDay. Rows that pass Gate A are
// stored whatever the day's completeness turns out to be.
func Validate(slots []Slot, opts ValidateOptions) ValidationResult {
	opts = opts.withDefaults()

	res := ValidationResult{
		Accepted: make([]Slot, 0, len(slots)),
	}
	for _, s := range slots {
		if rej, bad := checkStructure(s, opts); bad {
			res.Rejected = append(res.Rejected, rej)
			continue
		}
		res.Accepted = append(res.Accepted, s)
	}

	// Gate B runs only over what Gate A accepted. Measuring a jump against a
	// slot whose price we just refused to trust would raise a second alert
	// about the same broken row and hide the real neighbour relationship.
	for _, s := range res.Accepted {
		res.Warnings = append(res.Warnings, checkLevel(s, opts)...)
	}
	res.Warnings = append(res.Warnings, checkJumps(res.Accepted, opts)...)
	return res
}

// checkStructure is Gate A. It reports the FIRST failing check, ordered so the
// most fundamental failure is the one named: a slot with a NaN price and a
// broken timestamp is reported as a broken timestamp, because the timestamp is
// what makes the price locatable at all.
func checkStructure(s Slot, opts ValidateOptions) (Rejection, bool) {
	reject := func(reason RejectReason, format string, args ...any) (Rejection, bool) {
		return Rejection{Slot: s, Reason: reason, Detail: fmt.Sprintf(format, args...)}, true
	}

	if s.ValidFrom.IsZero() {
		return reject(ReasonValidFromZero,
			"valid_from is the zero time; a slot with no start cannot be priced or keyed")
	}
	// The API sends Z and FromRate coerces to UTC, so a non-UTC ValidFrom means
	// the slot came from somewhere else. It is refused rather than converted:
	// the key is a timestamp, and a zone-carrying key would compare equal to
	// itself and unequal in the store.
	if s.ValidFrom.Location() != time.UTC {
		return reject(ReasonValidFromNotUTC,
			"valid_from %s is in %s, not UTC", s.ValidFrom.Format(time.RFC3339), s.ValidFrom.Location())
	}
	// Agile boundaries are always :00 or :30. A drifted boundary means we have
	// misread the feed, and since the archive's key IS the boundary, a drifted
	// one would write a row no lookup ever finds.
	if !onHalfHour(s.ValidFrom) {
		return reject(ReasonValidFromUnaligned,
			"valid_from %s is not on a :00 or :30 boundary with zero seconds and nanoseconds",
			s.ValidFrom.Format(time.RFC3339Nano))
	}

	if s.ValidTo != nil {
		// Open-ended is legal and handled by the nil branch. A bounded interval
		// that ends at or before it starts covers no time at all, so it can
		// never price a kWh while still occupying its key.
		if !s.ValidTo.After(s.ValidFrom) {
			return reject(ReasonValidToNotAfter,
				"valid_to %s is not after valid_from %s",
				s.ValidTo.Format(time.RFC3339), s.ValidFrom.Format(time.RFC3339))
		}
		if opts.SlotDuration != AnyDuration && s.Duration() != opts.SlotDuration {
			return reject(ReasonSlotDuration,
				"interval is %s, want exactly %s; a silently changed slot length would "+
					"misalign every window we price", s.Duration(), opts.SlotDuration)
		}
	}

	// Finiteness is checked before the VAT relationship so a NaN is reported as
	// a NaN. NaN compares false against everything, so it would otherwise fail
	// the VAT check and be blamed on VAT.
	if err := finitePrices(s); err != nil {
		return reject(ReasonPriceNotFinite, "%s", err)
	}

	// One assertion that catches two different disasters: a VAT rate change we
	// have not been told about, and a unit error (pence read as pounds) in
	// either column. ✅ Verified exactly ×1.05 across 1,440 consecutive slots
	// including negative ones — exc −3.680 → inc −3.8640, because VAT on a
	// negative price makes it MORE negative, not less.
	want := s.ExcVATPence * (1 + opts.VATRate)
	if diff := math.Abs(s.IncVATPence - want); diff > opts.VATEpsilonPence {
		return reject(ReasonVATMismatch,
			"inc %.6fp but exc %.6fp at VAT %.4f implies %.6fp (off by %.6fp, tolerance %g)",
			s.IncVATPence, s.ExcVATPence, opts.VATRate, want, diff, opts.VATEpsilonPence)
	}

	// Parsed, not pattern-matched, and by the same function that builds request
	// paths — so a code this archive accepts is a code we could have fetched.
	if _, err := octopus.ParseTariffCode(s.TariffCode); err != nil {
		return reject(ReasonTariffCode, "%s", err)
	}
	if opts.TariffCode != "" && s.TariffCode != opts.TariffCode {
		return reject(ReasonTariffMismatch,
			"slot carries tariff %q but we requested %q; archiving it would bill this "+
				"house on another tariff's prices", s.TariffCode, opts.TariffCode)
	}

	return Rejection{}, false
}

// checkLevel is the per-slot half of Gate B: cap and floor, never a rejection.
//
// Plunge pricing is the entire point of Agile, so a collector that refused
// surprising prices would discard exactly the slots worth knowing about. The
// warning exists to wake somebody, not to filter.
func checkLevel(s Slot, opts ValidateOptions) []Warning {
	var out []Warning
	if s.IncVATPence > opts.MaxIncVATPence {
		out = append(out, Warning{Slot: s, Kind: WarnAboveCap, Detail: fmt.Sprintf(
			"inc-VAT price %.4fp exceeds the documented cap of %.4fp", s.IncVATPence, opts.MaxIncVATPence)})
	}
	if s.IncVATPence < opts.MinIncVATPence {
		out = append(out, Warning{Slot: s, Kind: WarnBelowFloor, Detail: fmt.Sprintf(
			"inc-VAT price %.4fp is below the sanity floor of %.4fp", s.IncVATPence, opts.MinIncVATPence)})
	}
	return out
}

// checkJumps is the set-aware half of Gate B.
//
// Slots are grouped by tariff AND payment method first. On a variable tariff the
// same interval appears twice, once per payment method, at different prices —
// comparing across that pair would report a "jump" on every flat tariff in the
// archive. Within a group only TRULY adjacent slots are compared: a step across
// a gap is not a step in the price curve, it is a missing slot, which is Gate
// C's business.
//
// The input is not reordered; a copy is sorted, because Accepted's order is part
// of Validate's contract.
func checkJumps(accepted []Slot, opts ValidateOptions) []Warning {
	groups := map[Key][]Slot{}
	for _, s := range accepted {
		k := Key{TariffCode: s.TariffCode, PaymentMethod: s.PaymentMethod}
		groups[k] = append(groups[k], s)
	}

	// Stable iteration so the warnings a given batch produces are identical run
	// to run; a log line that reorders itself is a log line nobody can diff.
	keys := make([]Key, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].TariffCode != keys[j].TariffCode {
			return keys[i].TariffCode < keys[j].TariffCode
		}
		return keys[i].PaymentMethod < keys[j].PaymentMethod
	})

	var out []Warning
	for _, k := range keys {
		g := append([]Slot(nil), groups[k]...)
		sort.SliceStable(g, func(i, j int) bool { return g[i].ValidFrom.Before(g[j].ValidFrom) })

		for i := 1; i < len(g); i++ {
			prev, cur := g[i-1], g[i]
			if prev.ValidTo == nil || !prev.ValidTo.Equal(cur.ValidFrom) {
				continue
			}
			if d := math.Abs(cur.IncVATPence - prev.IncVATPence); d > opts.MaxJumpPence {
				out = append(out, Warning{Slot: cur, Kind: WarnJump, Detail: fmt.Sprintf(
					"inc-VAT price moved %.4fp from %.4fp to %.4fp between %s and %s, "+
						"beyond the %.4fp threshold",
					d, prev.IncVATPence, cur.IncVATPence,
					prev.ValidFrom.Format(time.RFC3339), cur.ValidFrom.Format(time.RFC3339),
					opts.MaxJumpPence)})
			}
		}
	}
	return out
}

// onHalfHour reports whether t sits exactly on a :00 or :30 boundary. Seconds
// and nanoseconds are included: a slot one nanosecond off looks aligned in every
// log line and still fails to match the key a lookup computes.
func onHalfHour(t time.Time) bool {
	u := t.UTC()
	return u.Minute()%30 == 0 && u.Second() == 0 && u.Nanosecond() == 0
}

// finitePrices rejects NaN and ±Inf in either column. A NaN price propagates
// into money — every sum it touches becomes NaN — and poisons a bill silently,
// because NaN does not compare greater than any threshold a later check might
// apply.
func finitePrices(s Slot) error {
	for _, v := range []struct {
		name  string
		value float64
	}{{"exc-VAT", s.ExcVATPence}, {"inc-VAT", s.IncVATPence}} {
		if math.IsNaN(v.value) {
			return fmt.Errorf("%s price is NaN", v.name)
		}
		if math.IsInf(v.value, 0) {
			return fmt.Errorf("%s price is %v", v.name, v.value)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Gate C — set level, per LOCAL day.
// ---------------------------------------------------------------------------

// DayCompleteness is the verdict on one local day.
//
// It gates the SIGNAL, not the rows: every slot that passed Gate A is stored
// whatever this says. What it decides is whether the day may count towards
// complete_to — i.e. whether the collector can stop polling and whether
// the API may advertise the day as whole.
type DayCompleteness struct {
	// Day is local midnight IN the location asked about, so it prints as the
	// day a human means. Start and End are the same boundary as UTC instants,
	// which is what the store is queried with.
	Day   time.Time
	Start time.Time
	End   time.Time

	// Expected comes from the local calendar, never from the constant 48.
	Expected int
	// Present counts the slots actually held inside the window, duplicates
	// included — so a day with a duplicate and a hole cannot look complete by
	// arithmetic.
	Present int

	// Missing lists the expected slot starts (UTC) that are absent, oldest
	// first. Overlaps lists the starts of slots that begin before the previous
	// slot has ended, which is two prices for one kWh.
	Missing  []time.Time
	Overlaps []time.Time

	Complete bool
}

// MissingTailOnly reports whether the absent slots are exactly a contiguous run
// at the END of the day.
//
// This is the operationally useful distinction, and ✅ the observed case: at
// 16:08Z on 2026-09-10 the API's horizon claimed all of local 2026-09-11, but
// the day held 46 of its 48 slots — the 23:00 and 23:30 BST slots had not landed
// yet. A tail gap means keep polling; the publication is still arriving. An
// interior hole means polling will never fix it and somebody should look.
//
// With nothing missing there is no gap to describe, so this is false — ask
// Complete instead.
func (d DayCompleteness) MissingTailOnly() bool {
	if len(d.Missing) == 0 {
		return false
	}
	// Walk back from the end of the day: every missing slot must be one of the
	// last len(Missing) positions on the grid.
	want := d.End.Add(-DefaultSlotDuration * time.Duration(len(d.Missing)))
	for _, m := range d.Missing {
		if !m.Equal(want) {
			return false
		}
		want = want.Add(DefaultSlotDuration)
	}
	return true
}

// LocalDayWindow returns the UTC instants bounding the local day containing t.
//
// The boundary is NOT midnight UTC, and that is the whole point: local
// 2026-09-11 in London runs 2026-09-10T23:00Z → 2026-09-11T23:00Z, so a
// UTC-day window would mix two local days and mis-count both.
//
// A nil loc is treated as UTC. That is a programming error rather than a
// supported mode — in UTC no day is ever 46 or 50 slots long, so DST days would
// silently validate as wrong — and the collector's config refuses to start
// without a zone.
func LocalDayWindow(t time.Time, loc *time.Location) (start, end time.Time) {
	if loc == nil {
		loc = time.UTC
	}
	local := t.In(loc)
	start = time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
	// Adding 24h would be wrong on exactly the two days that matter. Asking the
	// calendar for the next day's midnight lets the zone decide how long this
	// day was.
	end = time.Date(local.Year(), local.Month(), local.Day()+1, 0, 0, 0, 0, loc)
	return start.UTC(), end.UTC()
}

// ExpectedSlots returns how many half-hourly slots the local day containing t
// holds.
//
// ✅ This is 46, 48 or 50 — never assume 48. 2026-03-29 is a 23-hour local day
// (46 slots) and 2025-10-26 is a 25-hour one (50). A hardcoded 48 would declare
// one of those days permanently incomplete and the other permanently corrupt,
// twice a year, on the two days of the year hardest to debug.
func ExpectedSlots(t time.Time, loc *time.Location) int {
	start, end := LocalDayWindow(t, loc)
	return int(end.Sub(start) / DefaultSlotDuration)
}

// CheckDay is Gate C: does the local day containing day hold a contiguous, fully
// populated set of slots?
//
// Slots outside the day's window are ignored rather than counted or complained
// about — a fetch spans whatever window was asked for, so neighbouring days'
// slots are routinely in the same batch.
//
// It expects slots for ONE tariff and ONE payment method, already through Gate
// A. Hand it a variable tariff's two payment methods together and every interval
// will read as an overlap, correctly: two prices for one kWh is exactly what
// that would be if they really shared a key.
func CheckDay(slots []Slot, day time.Time, loc *time.Location) DayCompleteness {
	start, end := LocalDayWindow(day, loc)
	if loc == nil {
		loc = time.UTC
	}

	d := DayCompleteness{
		Day:      start.In(loc),
		Start:    start,
		End:      end,
		Expected: int(end.Sub(start) / DefaultSlotDuration),
	}

	inDay := make([]Slot, 0, len(slots))
	for _, s := range slots {
		if !s.ValidFrom.Before(start) && s.ValidFrom.Before(end) {
			inDay = append(inDay, s)
		}
	}
	d.Present = len(inDay)
	sort.SliceStable(inDay, func(i, j int) bool { return inDay[i].ValidFrom.Before(inDay[j].ValidFrom) })

	have := make(map[int64]bool, len(inDay))
	for _, s := range inDay {
		have[s.ValidFrom.UnixNano()] = true
	}
	for t := start; t.Before(end); t = t.Add(DefaultSlotDuration) {
		if !have[t.UnixNano()] {
			d.Missing = append(d.Missing, t)
		}
	}

	// Overlap detection carries the furthest end seen so far rather than just
	// the previous slot's, so one long slot swallowing several neighbours
	// reports each of them.
	var reach time.Time
	for i, s := range inDay {
		slotEnd := end
		if s.ValidTo != nil {
			slotEnd = *s.ValidTo
		}
		if i > 0 && s.ValidFrom.Before(reach) {
			d.Overlaps = append(d.Overlaps, s.ValidFrom)
		}
		if i == 0 || slotEnd.After(reach) {
			reach = slotEnd
		}
	}

	// Present == Expected is redundant with an empty Missing for well-formed
	// input, and deliberately kept: it is the one check that still fails if a
	// slot lands in the window without sitting on the half-hourly grid.
	d.Complete = len(d.Missing) == 0 && len(d.Overlaps) == 0 && d.Present == d.Expected
	return d
}
