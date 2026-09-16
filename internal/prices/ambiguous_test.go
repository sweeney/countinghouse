package prices

import (
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// The key defends payment_method on write; the read path ignored it.
//
// Range has no filter and orders by payment_method, Curve.byStart was keyed on
// start alone, and Schedule.at returns the last covering charge — so two rows for
// one half hour silently became one at whichever sorted last, with the interval
// emitted twice in slots[] and rank/percentile computed over a duplicated
// population.
//
// The fix is not to pick a row on the reader's behalf — which payment method the
// account is on is a config fact this service does not have — nor to refuse the
// rows on write, which would make the composite key pointless and is unenforceable
// across separate Put calls anyway. An interval with two prices is an interval
// whose price we DO NOT KNOW, and Pricer's (float64, bool) already says exactly
// that. So it degrades like any other missing price: unpriced, visible, never
// silently wrong.
// ---------------------------------------------------------------------------

func ambigSlot(from time.Time, method string, exc float64) Slot {
	to := from.Add(SlotLength)
	return Slot{
		TariffCode: "E-1R-VAR-22-11-01-A", PaymentMethod: method,
		ValidFrom: from, ValidTo: &to,
		ExcVATPence: exc, IncVATPence: exc * 1.05, RetrievedAt: from,
	}
}

func TestTwoPaymentMethodsForOneSlotPriceAsUnknown(t *testing.T) {
	from := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	next := from.Add(SlotLength)
	c := NewCurve(from, next.Add(SlotLength), []Slot{
		ambigSlot(from, "DIRECT_DEBIT", 27.4113),
		ambigSlot(from, "NON_DIRECT_DEBIT", 28.95522),
		ambigSlot(next, "DIRECT_DEBIT", 30.0),
	})

	if _, ok := c.RateAt(from.Add(5 * time.Minute)); ok {
		t.Error("priced an interval that has two different prices; it must report unknown")
	}
	// The unambiguous neighbour is unaffected.
	if r, ok := c.RateAt(next.Add(5 * time.Minute)); !ok || r <= 0 {
		t.Errorf("neighbouring slot should still price: %v %v", r, ok)
	}
}

// Summary, rank and median must be computed over real intervals, not a
// duplicated population.
func TestAmbiguousSlotsAreNotCountedTwiceInTheSummary(t *testing.T) {
	from := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	next := from.Add(SlotLength)
	c := NewCurve(from, next.Add(SlotLength), []Slot{
		ambigSlot(from, "DIRECT_DEBIT", 27.4113),
		ambigSlot(from, "NON_DIRECT_DEBIT", 28.95522),
		ambigSlot(next, "DIRECT_DEBIT", 30.0),
	})
	if got := c.Summary().Slots; got != 1 {
		t.Errorf("Summary.Slots = %d; one half hour is priceable, not %d", got, got)
	}
	if n := len(c.Priced()); n != 1 {
		t.Errorf("Priced() returned %d slots, want 1", n)
	}
}

// And it reads as a gap, because that is what it is.
func TestAmbiguousSlotsMakeTheWindowIncomplete(t *testing.T) {
	from := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	next := from.Add(SlotLength)
	c := NewCurve(from, next.Add(SlotLength), []Slot{
		ambigSlot(from, "DIRECT_DEBIT", 27.4113),
		ambigSlot(from, "NON_DIRECT_DEBIT", 28.95522),
		ambigSlot(next, "DIRECT_DEBIT", 30.0),
	})
	if c.Complete() {
		t.Error("Complete() is true although one interval could not be priced")
	}
}

// One payment method is untouched — including the empty string Agile sends.
func TestASinglePaymentMethodIsUnaffected(t *testing.T) {
	from := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	for _, m := range []string{"", "DIRECT_DEBIT"} {
		c := NewCurve(from, from.Add(2*SlotLength), []Slot{
			ambigSlot(from, m, 20), ambigSlot(from.Add(SlotLength), m, 21),
		})
		if c.Summary().Slots != 2 || !c.Complete() {
			t.Errorf("method %q: slots=%d complete=%v, want 2/true",
				m, c.Summary().Slots, c.Complete())
		}
	}
}

// The daily relation has the same shape: Schedule.at returns the last covering
// charge, so two charges starting together at different prices would silently
// resolve to one. An unnameable standing charge must refuse, not guess.
func TestTwoStandingChargesForOneStartAreRefused(t *testing.T) {
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := from.Add(24 * time.Hour)
	mk := func(method string, inc float64) DailyCharge {
		return DailyCharge{TariffCode: "E-1R-VAR-22-11-01-A", PaymentMethod: method,
			ValidFrom: from, ValidTo: &end, ExcVATPence: inc / 1.05, IncVATPence: inc,
			RetrievedAt: from}
	}
	s := NewSchedule([]DailyCharge{mk("DIRECT_DEBIT", 56.19), mk("NON_DIRECT_DEBIT", 61.42)})

	if _, ok := s.PencePerDayAt(from.Add(time.Hour)); ok {
		t.Error("resolved a standing charge that has two different values")
	}
	if _, ok := s.ChargeOver(from, end); ok {
		t.Error("ChargeOver must refuse a window it cannot price rather than guess")
	}
}
