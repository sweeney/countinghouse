package prices

import (
	"fmt"
	"math"
	"time"

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
// As with Slot, inc is never derived from exc. That is the whole point of
// archiving these: the supplier's inc-VAT figure is what they bill, so a VAT
// change — such as the temporary zero rate on domestic electricity in Great
// Britain, 1 Oct 2026 to 31 Mar 2027 — arrives in the data rather than having to
// be applied from configuration.
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
}

// NewSchedule builds a schedule, sorting defensively: the archive returns rows
// in order, but a caller assembling them by hand is not obliged to.
func NewSchedule(charges []DailyCharge) Schedule {
	// Same reasoning as Curve's dropAmbiguous, and the same resolution: at() scans
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
// Elapsed time, not calendar days: a window spanning a DST changeover lasted 23
// or 25 hours and is charged for what it actually lasted, exactly as
// config.Segment.Days() does.
func (s Schedule) ChargeOver(from, to time.Time) (float64, bool) {
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
		days := stop.Sub(cursor).Hours() / 24
		total += days * c.IncVATPence / 100
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
// input into a checkable expectation.
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
	rate, ok := opts.expectedVATAt(c.ValidFrom)
	if !ok {
		return Warning{}, false
	}
	want := c.ExcVATPence * (1 + rate)
	diff := math.Abs(c.IncVATPence - want)
	if diff <= opts.VATEpsilonPence {
		return Warning{}, false
	}
	w := Warning{
		Slot: c.row(), Kind: WarnVATMismatch,
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

func isNotFinite(v float64) bool { return math.IsNaN(v) || math.IsInf(v, 0) }

// dropAmbiguousCharges removes standing charges that start at the same instant
// with different prices — the payment-method collapse, on the daily relation.
func dropAmbiguousCharges(charges []DailyCharge) []DailyCharge {
	rows := make([]Slot, len(charges))
	for i, c := range charges {
		rows[i] = c.row()
	}
	kept := dropAmbiguous(rows)
	if len(kept) == len(charges) {
		return charges
	}
	out := make([]DailyCharge, 0, len(kept))
	for _, r := range kept {
		out = append(out, dailyChargeFromRow(r))
	}
	return out
}
