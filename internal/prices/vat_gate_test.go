package prices

import (
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// The VAT relationship is a Gate B warning, not a Gate A rejection.
//
// The check itself is worth keeping: it catches a VAT change nobody told us about and
// a unit error (pence read as pounds) in either column, and it was verified as exactly
// x1.05 across 1,440 consecutive slots including negative ones.
//
// What changed is what a failure COSTS. Both columns come from the supplier and are
// self-consistent with each other; what disagrees is our own config's vat_rate. And the
// cost path never reads that config rate for a unit price — Curve.RateAt returns
// IncVATPence straight from the archive, so the supplier's delivered inc figure is
// already the authority. Rejecting the slot therefore discards correct supplier data to
// protect an assumption nothing downstream uses.
//
// Concretely: on a real VAT change, Gate A would reject every slot published after it
// until somebody edited config — so the window where prices matter most would be the
// window with no prices, while /healthz showed fetches succeeding. Gate B fails just as
// loudly and keeps the data.
//
// Gate A's own framing is "the value cannot be money". A price whose inc/exc ratio is
// not the one we expected is money; it is money we should be told about.
// ---------------------------------------------------------------------------

// vatSlot is one well-formed slot with the given ex/inc pence.
func vatSlot(exc, inc float64) Slot {
	from := time.Date(2026, 9, 13, 0, 0, 0, 0, time.UTC)
	to := from.Add(SlotLength)
	return Slot{
		TariffCode:  "E-1R-AGILE-24-10-01-A",
		ValidFrom:   from,
		ValidTo:     &to,
		ExcVATPence: exc,
		IncVATPence: inc,
		RetrievedAt: from,
	}
}

// A mismatch must be STORED and flagged, not rejected.
func TestVATMismatchIsWarnedNotRejected(t *testing.T) {
	// Supplier says 20% VAT; our config still says 5%.
	s := vatSlot(20.0, 24.0)
	res := Validate([]Slot{s}, ValidateOptions{VATRate: 0.05})

	if len(res.Rejected) != 0 {
		t.Errorf("slot rejected for %q — a correct supplier price must not be discarded "+
			"because our config disagrees about VAT: %s",
			res.Rejected[0].Reason, res.Rejected[0].Detail)
	}
	if len(res.Accepted) != 1 {
		t.Fatalf("accepted %d slots, want 1 — the price is real and must be archived", len(res.Accepted))
	}
	if len(res.Warnings) != 1 {
		t.Fatalf("warnings = %d, want 1; a VAT change must still be loud", len(res.Warnings))
	}
	w := res.Warnings[0]
	if w.Kind != WarnVATMismatch {
		t.Errorf("warning kind = %q, want %q", w.Kind, WarnVATMismatch)
	}
	// The IMPLIED rate belongs in the detail: it is the number that tells an operator
	// what to change config to, and it is also the per-slot VAT rate the archive
	// records implicitly.
	if !strings.Contains(w.Detail, "0.2") {
		t.Errorf("detail does not report the implied rate (0.20): %q", w.Detail)
	}
}

// The check still catches a unit error, which is the other disaster it exists for —
// and that one is far outside any plausible VAT rate.
func TestVATCheckStillCatchesAUnitError(t *testing.T) {
	// exc in pence, inc accidentally in POUNDS.
	res := Validate([]Slot{vatSlot(20.0, 0.21)}, ValidateOptions{VATRate: 0.05})
	if len(res.Warnings) != 1 {
		t.Fatalf("warnings = %d, want 1 for a pence/pounds mix-up", len(res.Warnings))
	}
	if len(res.Accepted) != 1 {
		t.Errorf("accepted = %d; even a suspicious price is archived and flagged, because "+
			"discarding it loses the evidence", len(res.Accepted))
	}
}

// A matching relationship produces neither.
func TestVATMatchIsSilent(t *testing.T) {
	res := Validate([]Slot{vatSlot(20.0, 21.0)}, ValidateOptions{VATRate: 0.05})
	if len(res.Warnings) != 0 {
		t.Errorf("warnings = %d, want 0: 20.0 x 1.05 = 21.0", len(res.Warnings))
	}
	if len(res.Rejected) != 0 {
		t.Errorf("rejected = %d, want 0", len(res.Rejected))
	}
}

// Negative prices: VAT on a negative price makes it MORE negative. This held across
// the verified 1,440-slot run and must survive the move between gates.
func TestVATCheckOnNegativePrices(t *testing.T) {
	// exc -3.680 -> inc -3.8640 at 5%.
	res := Validate([]Slot{vatSlot(-3.680, -3.8640)}, ValidateOptions{VATRate: 0.05})
	if len(res.Warnings) != 0 || len(res.Rejected) != 0 {
		t.Errorf("a correct negative pair was flagged: warnings=%d rejected=%d",
			len(res.Warnings), len(res.Rejected))
	}
	// And the sign error — VAT making a negative price less negative — is caught.
	res = Validate([]Slot{vatSlot(-3.680, -3.4960)}, ValidateOptions{VATRate: 0.05})
	if len(res.Warnings) != 1 {
		t.Errorf("VAT applied in the wrong direction on a negative price was not flagged")
	}
}

// An UNKNOWN expected VAT rate means no opinion, not an opinion of zero.
//
// Zero is a real rate, so it cannot double as "unset": validating against 0 makes the
// implied rate wrong for every slot and, under the old Gate A, rejected an entire
// backfill. With the check as a warning it would instead flag every slot, which is a
// different way to make the signal useless.
func TestUnknownVATRateSkipsTheCheck(t *testing.T) {
	res := Validate([]Slot{vatSlot(20.0, 21.0)}, ValidateOptions{}) // VATRate unset
	if len(res.Warnings) != 0 {
		t.Errorf("warnings = %d with no expected VAT rate; an unknown rate must express "+
			"no opinion rather than assume 0%%: %+v", len(res.Warnings), res.Warnings)
	}
	if len(res.Accepted) != 1 {
		t.Errorf("accepted = %d, want 1", len(res.Accepted))
	}
}

// A genuinely zero-rated tariff must still be checkable, so "unset" has to be
// expressible separately from 0.
func TestAnExplicitZeroVATRateIsStillChecked(t *testing.T) {
	zero := 0.0
	// At 0% VAT, inc must equal exc; this slot says otherwise.
	res := Validate([]Slot{vatSlot(20.0, 21.0)}, ValidateOptions{ExpectVATRate: &zero})
	if len(res.Warnings) != 1 {
		t.Errorf("warnings = %d, want 1: at an explicit 0%% VAT, inc must equal exc", len(res.Warnings))
	}
}

// ---------------------------------------------------------------------------
// Per-slot VAT resolution.
//
// One rate for a whole batch is wrong in two ways the collector actually hits: a
// backfill spans agreements, and a superseded tariff's VAT may differ from today's. The
// expected rate therefore has to be resolved from the agreement covering EACH slot's
// valid_from, not from one instant chosen at boot.
// ---------------------------------------------------------------------------

func TestExpectVATRateAtResolvesPerSlot(t *testing.T) {
	boundary := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)

	// 5% before noon, 20% after — a VAT change mid-batch.
	opts := ValidateOptions{
		ExpectVATRateAt: func(at time.Time) (float64, bool) {
			if at.Before(boundary) {
				return 0.05, true
			}
			return 0.20, true
		},
	}

	at := func(h, m int) time.Time { return time.Date(2026, 9, 13, h, m, 0, 0, time.UTC) }
	slot := func(from time.Time, exc, inc float64) Slot {
		to := from.Add(SlotLength)
		return Slot{
			TariffCode: "E-1R-AGILE-24-10-01-A", ValidFrom: from, ValidTo: &to,
			ExcVATPence: exc, IncVATPence: inc, RetrievedAt: from,
		}
	}

	// Each slot is correct under ITS OWN rate. A single-rate check would flag half.
	in := []Slot{
		slot(at(11, 0), 20, 21),  // 5%
		slot(at(11, 30), 20, 21), // 5%
		slot(at(12, 0), 20, 24),  // 20%
		slot(at(12, 30), 20, 24), // 20%
	}
	res := Validate(in, opts)
	if len(res.Warnings) != 0 {
		t.Errorf("warnings = %d, want 0 — every slot is correct under the rate in force "+
			"at its own valid_from: %+v", len(res.Warnings), res.Warnings)
	}
	if len(res.Accepted) != 4 {
		t.Errorf("accepted = %d, want 4", len(res.Accepted))
	}

	// And a slot that is wrong under its own rate is still caught.
	bad := Validate([]Slot{slot(at(13, 0), 20, 21)}, opts) // 5% figures after the change
	if len(bad.Warnings) != 1 {
		t.Errorf("a slot at the old rate after the change drew %d warnings, want 1", len(bad.Warnings))
	}
}

// A resolver that cannot name the rate for a given slot expresses no opinion for THAT
// slot, while still checking the ones it can. Boot during an agreement gap and a
// backfill reaching before the first agreement both produce this.
func TestExpectVATRateAtUnknownForSomeSlots(t *testing.T) {
	known := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	opts := ValidateOptions{
		ExpectVATRateAt: func(at time.Time) (float64, bool) {
			if at.Before(known) {
				return 0, false // no agreement covers this
			}
			return 0.05, true
		},
	}
	slot := func(from time.Time, exc, inc float64) Slot {
		to := from.Add(SlotLength)
		return Slot{
			TariffCode: "E-1R-AGILE-24-10-01-A", ValidFrom: from, ValidTo: &to,
			ExcVATPence: exc, IncVATPence: inc, RetrievedAt: from,
		}
	}

	res := Validate([]Slot{
		// Before the first agreement: an odd ratio, but we have no basis for an
		// opinion, so saying nothing is the honest answer.
		slot(time.Date(2026, 9, 13, 11, 0, 0, 0, time.UTC), 20, 24),
		// Covered, and wrong under the known rate.
		slot(time.Date(2026, 9, 13, 13, 0, 0, 0, time.UTC), 20, 24),
	}, opts)

	if len(res.Accepted) != 2 {
		t.Fatalf("accepted = %d, want 2", len(res.Accepted))
	}
	if len(res.Warnings) != 1 {
		t.Fatalf("warnings = %d, want exactly 1 — the uncovered slot must draw none and "+
			"the covered one must still be checked: %+v", len(res.Warnings), res.Warnings)
	}
	if !res.Warnings[0].ValidFromEquals(time.Date(2026, 9, 13, 13, 0, 0, 0, time.UTC)) {
		t.Errorf("the warning is on the wrong slot: %s", res.Warnings[0].Slot.ValidFrom)
	}
}

// ValidFromEquals is a small readability helper for the assertion above.
func (w Warning) ValidFromEquals(t time.Time) bool { return w.Slot.ValidFrom.Equal(t) }

// The resolver takes precedence over both constant forms, so a caller that can resolve
// per slot is never silently overridden by a leftover scalar.
func TestExpectVATRateAtWinsOverTheConstants(t *testing.T) {
	rate := 0.99
	opts := ValidateOptions{
		VATRate:         0.99,
		ExpectVATRate:   &rate,
		ExpectVATRateAt: func(time.Time) (float64, bool) { return 0.05, true },
	}
	if len(Validate([]Slot{vatSlot(20, 21)}, opts).Warnings) != 0 {
		t.Error("the per-slot resolver was ignored in favour of a constant")
	}
}
