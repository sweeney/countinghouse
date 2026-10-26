package prices

import (
	"fmt"
	"math"
	"time"

	"github.com/sweeney/countinghouse/internal/calendar"
	"github.com/sweeney/countinghouse/internal/octopus"
)

// DailyCharge is one standing-charge period of one tariff, as archived.
//
// Structurally identical to Slot, and deliberately a DIFFERENT TYPE: the unit
// here is pence per DAY, there it is pence per kWh. Sharing one type would make
// it possible to add a standing charge to a unit price and get a number, which
// is exactly the class of mistake this service spends its effort preventing.
// The STORAGE path is shared, so the bitemporal machinery has one implementation.
//
// As with Slot, inc is never derived from exc when ARCHIVED. The supplier's
// inc-VAT figure is what they bill, so a VAT change — such as the temporary zero
// rate on domestic electricity in Great Britain, 1 Oct 2026 to 31 Mar 2027 —
// arrives in the data rather than having to be applied from configuration. The
// one place a bill does derive it is ChargeOverAtVAT, for days the archive never
// saw a figure for and the figure it holds is plainly not true of.
type DailyCharge struct {
	TariffCode    string
	PaymentMethod string

	ValidFrom time.Time
	ValidTo   *time.Time

	ExcVATPence float64
	IncVATPence float64

	RetrievedAt time.Time
}

// Covers reports whether t falls in [ValidFrom, ValidTo). An open-ended charge
// covers everything from ValidFrom onwards — which is the normal state of the
// current standing charge, not an edge case.
func (d DailyCharge) Covers(t time.Time) bool {
	if t.Before(d.ValidFrom) {
		return false
	}
	return d.ValidTo == nil || t.Before(*d.ValidTo)
}

// row converts to the shared storage shape.
//
// The two structs are field-for-field identical, so this is a plain conversion —
// and having to WRITE the conversion is the point. Go will not let a DailyCharge
// be used where a Slot is expected without it, so pence-per-day cannot drift into
// pence-per-kWh by accident; only on purpose, here and in dailyChargeFromRow.
func (d DailyCharge) row() Slot { return Slot(d) }

func dailyChargeFromRow(s Slot) DailyCharge { return DailyCharge(s) }

// DailyChargeFromRate converts one API rate into an archivable standing charge.
func DailyChargeFromRate(tariffCode string, r octopus.Rate, retrievedAt time.Time) DailyCharge {
	return DailyCharge{
		TariffCode:    tariffCode,
		PaymentMethod: r.PaymentMethod,
		ValidFrom:     r.ValidFrom.UTC(),
		ValidTo:       utcPtr(r.ValidTo),
		ExcVATPence:   r.ExcVATPence,
		IncVATPence:   r.IncVATPence,
		RetrievedAt:   retrievedAt.UTC(),
	}
}

// DailyChargesFromRates converts a batch, sharing one retrievedAt so the whole
// fetch is attributable to a single instant.
func DailyChargesFromRates(tariffCode string, rates []octopus.Rate, retrievedAt time.Time) []DailyCharge {
	out := make([]DailyCharge, 0, len(rates))
	for _, r := range rates {
		out = append(out, DailyChargeFromRate(tariffCode, r, retrievedAt))
	}
	return out
}

// Schedule is the standing charges in force across a stretch of time, and knows
// how to charge a window against them.
//
// It is the standing-charge counterpart of Curve, and like Curve's Pricer it can
// say "I don't know": a window it does not fully cover yields ok=false rather
// than a number. Returning a partial total would under-bill silently, which is
// the failure this whole layer exists to avoid.
type Schedule struct {
	charges []DailyCharge // ascending by ValidFrom
	eras    map[Key][]era // per charge; nil when built without history
}

// NewSchedule builds a schedule, sorting defensively: the archive returns rows
// in order, but a caller assembling them by hand is not obliged to.
func NewSchedule(charges []DailyCharge) Schedule {
	// Same reasoning as Curve's oneRowPerInterval, and the same resolution: at() scans
	// backwards and returns the LAST covering charge, so two charges starting at the
	// same instant at different prices would silently resolve to whichever sorted
	// last. A standing charge we cannot name is one ChargeOver must refuse rather
	// than guess, which it already does for any gap.
	charges = dropAmbiguousCharges(charges)
	cs := make([]DailyCharge, len(charges))
	copy(cs, charges)
	for i := 1; i < len(cs); i++ {
		for j := i; j > 0 && cs[j].ValidFrom.Before(cs[j-1].ValidFrom); j-- {
			cs[j], cs[j-1] = cs[j-1], cs[j]
		}
	}
	return Schedule{charges: cs}
}

// Empty reports whether the schedule holds nothing, so a caller can fall back to
// configuration without first constructing a window.
func (s Schedule) Empty() bool { return len(s.charges) == 0 }

// PencePerDayAt returns the VAT-INCLUSIVE pence per day in force at t.
func (s Schedule) PencePerDayAt(t time.Time) (float64, bool) {
	c, ok := s.at(t)
	if !ok {
		return 0, false
	}
	return c.IncVATPence, true
}

func (s Schedule) at(t time.Time) (DailyCharge, bool) {
	// Latest-starting charge that covers t. Linear because a tariff has a handful
	// of standing-charge periods in its whole life, not tens of thousands.
	for i := len(s.charges) - 1; i >= 0; i-- {
		if s.charges[i].Covers(t) {
			return s.charges[i], true
		}
	}
	return DailyCharge{}, false
}

// ChargeOver returns the VAT-inclusive GBP standing charge for [from, to),
// apportioned across every rate in force during it.
//
// Calendar days, not elapsed time (issue #56). A standing charge is a price per
// calendar day of supply, so a whole local day is charged as one day whether it
// lasted 23, 24 or 25 hours — which is how the supplier bills it. A part day is
// pro rata against the length of the day it is part of. loc is the zone whose
// calendar defines "a day" (the house zone, Europe/London in prod); from and to
// may carry any Location.
//
// Each charge period's share is measured with calendar.Days, the same function
// config.Segment.Days uses, so the archive path and the configured-tariff path
// cannot disagree about what a day is. calendar.Days is additive, so a charge
// change landing mid-day splits that day between the two rates and the shares
// still sum to exactly one day.
//
// Previously this was stop.Sub(cursor).Hours()/24, deliberately — "charged for
// what it actually lasted" — which billed 25 Oct 2026 at 25/24 of a day.
func (s Schedule) ChargeOver(from, to time.Time, loc *time.Location) (float64, bool) {
	return s.charge(from, to, func(c DailyCharge, a, b time.Time) float64 {
		return calendar.Days(a, b, loc) * c.IncVATPence
	})
}

// VATRateTolerance is how far a supplier's implied VAT rate may sit from an
// expected one and still count as the same rate. The inputs are pence rounded by
// the supplier: at a standing charge of 56.19p a single rounded penny moves the
// implied rate by more than a thousandth.
const VATRateTolerance = 5e-4

// ImpliesVATRate reports whether the charge's own inc and exc figures imply
// rate, within VATRateTolerance.
//
// A charge with no positive ex-VAT figure implies nothing checkable and is taken
// at its word. That is deliberate rather than an oversight: for a real zero
// charge it is simply true, and for a missing or nonsensical ex-VAT figure there
// is nothing better to bill, since regrossing it would give 0 x (1 + rate).
func (d DailyCharge) ImpliesVATRate(rate float64) bool {
	if d.ExcVATPence <= 0 {
		return true
	}
	return math.Abs(d.IncVATPence/d.ExcVATPence-1-rate) <= VATRateTolerance
}

// ChargeHistory is an archived standing charge together with what the archive
// saw of it over time: when the row was first seen, and every in-place
// restatement of it, oldest first.
type ChargeHistory struct {
	Charge DailyCharge

	// FirstSeenAt is zero for a row that predates the first_seen_at column.
	FirstSeenAt time.Time

	Restatements []Restatement
}

// NewScheduleFromHistory builds a schedule that knows, for every day, which of
// the supplier's figures for a charge was current on that day.
//
// That matters because the supplier restates a standing charge IN PLACE. On
// 1 Oct 2026 Octopus rewrote the one Agile standing charge, open-ended from
// 2024, to its zero-rated figure (inc == exc). The row now says 0% VAT for every
// day back to 2024, and bills before the cut went out without VAT (#51). The
// archive kept the 5% figure in its restatement log, and that is what those
// days were billed at by the supplier at the time.
func NewScheduleFromHistory(hs []ChargeHistory) Schedule {
	charges := make([]DailyCharge, 0, len(hs))
	eras := make(map[Key][]era, len(hs))
	for _, h := range hs {
		charges = append(charges, h.Charge)
		eras[chargeKey(h.Charge)] = erasOf(h)
	}
	s := NewSchedule(charges)
	s.eras = eras
	return s
}

func chargeKey(c DailyCharge) Key {
	k := c.row().Key()
	k.ValidFrom = k.ValidFrom.UTC()
	return k
}

// eraKind is what the archive knows about the supplier's figure for a stretch of
// a charge's validity.
type eraKind int

const (
	// eraUnwitnessed: before the archive saw the charge. The figure is a later
	// one carried back, so its inc-VAT column is only trusted where it agrees
	// with the agreement covering the day.
	eraUnwitnessed eraKind = iota

	// eraWitnessed: the figure the supplier was publishing for these days while
	// they happened. Billed as-is, whatever configuration says.
	eraWitnessed

	// eraBetween: after the last confirmation of one figure and before the first
	// sighting of the next. The change happened somewhere in here, and the
	// agreement is the only thing that can say where.
	eraBetween
)

// era is one stretch of a charge's validity, from its start until the next
// era's. The first era of a charge starts at the zero time: since forever.
type era struct {
	from time.Time
	kind eraKind
	fig  DailyCharge
	alt  DailyCharge // eraBetween only: the figure after the restatement
}

// erasOf lays a charge's history out as eras, oldest first.
//
// A restatement that leaves the ex-VAT figure alone is a VAT change, and a VAT
// change is never retrospective: the old figure stays true of the days before
// it. A restatement that moves the ex-VAT figure is a correction, the
// supplier's account of every day the charge covers, so what came before it is
// discarded and the corrected figure counts as unwitnessed until it was seen.
func erasOf(h ChargeHistory) []era {
	fig := h.Charge
	if len(h.Restatements) > 0 {
		fig.ExcVATPence = h.Restatements[0].OldExcVATPence
		fig.IncVATPence = h.Restatements[0].OldIncVATPence
	}
	seen := h.FirstSeenAt
	if seen.IsZero() {
		// The row predates first_seen_at. It was certainly seen by the earliest
		// confirmation still on record.
		seen = h.Charge.RetrievedAt
		if len(h.Restatements) > 0 {
			seen = h.Restatements[0].PreviousRetrievedAt
		}
	}
	eras := []era{{kind: eraUnwitnessed, fig: fig}, {from: seen, kind: eraWitnessed, fig: fig}}
	for _, r := range h.Restatements {
		next := fig
		next.ExcVATPence, next.IncVATPence = r.NewExcVATPence, r.NewIncVATPence
		if math.Abs(r.NewExcVATPence-r.OldExcVATPence) > priceEpsilon {
			eras = []era{{kind: eraUnwitnessed, fig: next}, {from: r.DetectedAt, kind: eraWitnessed, fig: next}}
		} else {
			eras = append(eras,
				era{from: r.PreviousRetrievedAt, kind: eraBetween, fig: fig, alt: next},
				era{from: r.DetectedAt, kind: eraWitnessed, fig: next})
		}
		fig = next
	}
	// The log's timestamps are ordered by construction; clamp anyway, so a
	// damaged one shortens an era rather than making the walk go backwards.
	for i := 1; i < len(eras); i++ {
		if eras[i].from.Before(eras[i-1].from) {
			eras[i].from = eras[i-1].from
		}
	}
	return eras
}

// pence is the era's pence per day under an agreement at vatRate, and whether
// it had to be grossed up from ex-VAT to get it.
func (e era) pence(vatRate float64) (float64, bool) {
	switch e.kind {
	case eraWitnessed:
		return e.fig.IncVATPence, false
	case eraBetween:
		if e.fig.ImpliesVATRate(vatRate) {
			return e.fig.IncVATPence, false
		}
		if e.alt.ImpliesVATRate(vatRate) {
			return e.alt.IncVATPence, false
		}
		return e.alt.ExcVATPence * (1 + vatRate), true
	default:
		if e.fig.ImpliesVATRate(vatRate) {
			return e.fig.IncVATPence, false
		}
		return e.fig.ExcVATPence * (1 + vatRate), true
	}
}

// ChargeOverAtVAT is ChargeOver for a window one agreement covers, at that
// agreement's VAT rate.
//
// Each day is billed at the supplier's figure for that day where the archive
// witnessed one (see NewScheduleFromHistory), whatever the agreement says: the
// supplier is what gets paid, and a disagreeing agreement is the drift alert's
// business, not the bill's. Only for days the archive never saw a figure for
// does the agreement decide, and then only whether the carried-back inc-VAT
// figure is believable. Where it is not, the day is billed at the ex-VAT figure
// grossed up by vatRate, and regrossed says so.
//
// A schedule built without history knows of no witnessed days, so every
// disagreeing day is regrossed.
//
// Days are calendar days in loc, exactly as ChargeOver counts them: a whole
// 25- or 23-hour clock-change day is one day (issue #56).
func (s Schedule) ChargeOverAtVAT(from, to time.Time, vatRate float64, loc *time.Location) (total float64, regrossed bool, ok bool) {
	total, ok = s.charge(from, to, func(c DailyCharge, a, b time.Time) float64 {
		eras, known := s.eras[chargeKey(c)]
		if !known {
			eras = []era{{kind: eraUnwitnessed, fig: c}}
		}
		var pence float64
		for i, e := range eras {
			lo, hi := e.from, b
			if i+1 < len(eras) && eras[i+1].from.Before(b) {
				hi = eras[i+1].from
			}
			if lo.Before(a) {
				lo = a
			}
			if !hi.After(lo) {
				continue
			}
			p, rg := e.pence(vatRate)
			regrossed = regrossed || rg
			// Calendar days in loc (issue #56). calendar.Days is additive, so
			// cutting one charge into eras — even mid-way through a 25-hour
			// clock-change day — still sums to the days the charge covers.
			pence += calendar.Days(lo, hi, loc) * p
		}
		return pence
	})
	if !ok {
		return 0, false, false
	}
	return total, regrossed, true
}

// charge walks [from, to) across the charges in force, summing the pence-days
// bill returns for each stretch [a, b) of one charge.
func (s Schedule) charge(from, to time.Time, bill func(c DailyCharge, a, b time.Time) float64) (float64, bool) {
	if !to.After(from) {
		return 0, false
	}
	var total float64
	cursor := from
	for cursor.Before(to) {
		c, ok := s.at(cursor)
		if !ok {
			// A gap. Refusing beats returning the covered part, which would look
			// like a correct but suspiciously cheap bill.
			return 0, false
		}
		stop := to
		if c.ValidTo != nil && c.ValidTo.Before(stop) {
			stop = *c.ValidTo
		}
		// at() guarantees ValidFrom <= cursor < ValidTo, so stop is strictly after
		// cursor and this terminates.
		total += bill(c, cursor, stop) / 100
		cursor = stop
	}
	return total, true
}

// ValidateStandingCharges runs the same two gates over standing charges that
// Validate runs over prices, with the slot-grid checks dropped — a standing
// charge is not aligned to half hours and has no business being.
//
// Gate A rejects: a non-finite figure, an inverted interval, a tariff code we did
// not ask for. Gate B warns: the inc/exc relationship is not the VAT rate we
// expected. That warning is what turns configuration's vat_rate from a billing
// input into a checkable expectation: a bill uses the supplier's figure for every
// day the archive saw one, so config only decides days it never did.
func ValidateStandingCharges(charges []DailyCharge, opts ValidateOptions) StandingValidation {
	opts = opts.withDefaults()
	res := StandingValidation{Accepted: make([]DailyCharge, 0, len(charges))}

	for _, c := range charges {
		switch {
		case isNotFinite(c.ExcVATPence) || isNotFinite(c.IncVATPence):
			res.Rejected = append(res.Rejected, StandingRejection{Charge: c,
				Reason: ReasonPriceNotFinite,
				Detail: fmt.Sprintf("exc %v inc %v", c.ExcVATPence, c.IncVATPence)})
			continue
		case c.ValidTo != nil && !c.ValidTo.After(c.ValidFrom):
			res.Rejected = append(res.Rejected, StandingRejection{Charge: c,
				Reason: ReasonValidToNotAfter,
				Detail: fmt.Sprintf("valid_to %s is not after valid_from %s",
					c.ValidTo.Format(time.RFC3339), c.ValidFrom.Format(time.RFC3339))})
			continue
		case opts.TariffCode != "" && c.TariffCode != opts.TariffCode:
			res.Rejected = append(res.Rejected, StandingRejection{Charge: c,
				Reason: ReasonTariffMismatch,
				Detail: fmt.Sprintf("got %q, asked for %q", c.TariffCode, opts.TariffCode)})
			continue
		}
		res.Accepted = append(res.Accepted, c)

		if w, bad := checkStandingVAT(c, opts); bad {
			res.Warnings = append(res.Warnings, w)
		}
	}
	return res
}

// StandingValidation is the verdict on a batch of standing charges. Every input
// appears exactly once across Accepted and Rejected.
type StandingValidation struct {
	Accepted []DailyCharge
	Rejected []StandingRejection
	Warnings []Warning
}

// StandingRejection is one standing charge refused by Gate A, with why.
type StandingRejection struct {
	Charge DailyCharge
	Reason RejectReason
	Detail string
}

func checkStandingVAT(c DailyCharge, opts ValidateOptions) (Warning, bool) {
	rate, ok := opts.expectedVATAt(standingVATCheckedAt(c))
	if !ok {
		return Warning{}, false
	}
	want := c.ExcVATPence * (1 + rate)
	diff := math.Abs(c.IncVATPence - want)
	if diff <= opts.VATEpsilonPence {
		return Warning{}, false
	}
	w := Warning{
		Slot: c.row(), Kind: WarnVATMismatch, ExpectedVATRate: &rate,
		Detail: fmt.Sprintf(
			"standing charge: inc %.6fp/day but exc %.6fp/day at VAT %.4f implies %.6fp/day "+
				"(off by %.6fp, tolerance %g)",
			c.IncVATPence, c.ExcVATPence, rate, want, diff, opts.VATEpsilonPence),
	}
	if c.ExcVATPence != 0 {
		implied := c.IncVATPence/c.ExcVATPence - 1
		w.ImpliedVATRate = &implied
		w.Detail += fmt.Sprintf("; the supplier's figures imply VAT of %.4f", implied)
	}
	return w, true
}

// standingVATCheckedAt is the instant whose VAT a standing charge's inc figure is
// for: the moment it was retrieved, held within the charge's own validity.
//
// Not ValidFrom. The supplier grosses up an open-ended charge at the VAT in force
// when it is published, and restates it in place when that changes, so the
// current charge, valid from 2024, carries today's VAT. Judged at 2024 it would
// disagree on every daily sync for as long as the rate it replaced is gone.
func standingVATCheckedAt(c DailyCharge) time.Time {
	at := c.RetrievedAt
	if at.Before(c.ValidFrom) {
		return c.ValidFrom
	}
	if c.ValidTo != nil && !at.Before(*c.ValidTo) {
		return c.ValidTo.Add(-time.Nanosecond)
	}
	return at
}

func isNotFinite(v float64) bool { return math.IsNaN(v) || math.IsInf(v, 0) }

// dropAmbiguousCharges removes standing charges that start at the same instant
// with different prices — the payment-method collapse, on the daily relation.
func dropAmbiguousCharges(charges []DailyCharge) []DailyCharge {
	rows := make([]Slot, len(charges))
	for i, c := range charges {
		rows[i] = c.row()
	}
	// NewCurve does the detection; a schedule is small enough that reusing it is
	// simpler than a second implementation of the same rule.
	kept := NewCurve(time.Time{}, time.Time{}, rows).Slots
	if len(kept) == len(charges) {
		return charges
	}
	out := make([]DailyCharge, 0, len(kept))
	for _, r := range kept {
		out = append(out, dailyChargeFromRow(r))
	}
	return out
}
