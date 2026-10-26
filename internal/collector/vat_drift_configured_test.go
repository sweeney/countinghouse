package collector

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sweeney/countinghouse/internal/notify"
	"github.com/sweeney/countinghouse/internal/octopus"
	"github.com/sweeney/countinghouse/internal/prices"
	"github.com/sweeney/countinghouse/internal/testutil"
)

// ---------------------------------------------------------------------------
// Issue #52: agreement_drift said "the supplier is charging VAT at 0.00%, but
// configuration says 0.00%".
//
// The per-slot check compared the supplier against the rate the AGREEMENT
// covering each slot names (VATRateAt). The alert then reported "configuration"
// from the deprecated scalar VATRate, which production never sets — so it was
// always 0, and a real 0%-vs-5% disagreement printed as two identical figures.
// Every other test's harness sets both fields to 5%, which is why none saw it.
// ---------------------------------------------------------------------------

// newProductionWiredHarness builds a collector the way cmd/countinghouse does:
// VATRateAt only, the legacy VATRate left at its zero value.
func newProductionWiredHarness(t *testing.T, now time.Time, f RateFetcher, vatAt func(time.Time) (float64, bool)) *harness {
	t.Helper()
	store, err := prices.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { store.Close() }) //nolint:errcheck

	clock := testutil.NewFakeClock(now)
	noti := &recordingNotifier{}
	c, err := New(Options{
		Fetcher:    f,
		Store:      store,
		Notifier:   noti,
		Clock:      clock,
		Location:   london(t),
		TariffCode: testTariffCode,
		VATRateAt:  vatAt,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h := &harness{c: c, store: store, noti: noti, clock: clock}
	if ff, ok := f.(*fakeFetcher); ok {
		h.fetch = ff
	}
	return h
}

func constVAT(rate float64) func(time.Time) (float64, bool) {
	return func(time.Time) (float64, bool) { return rate, true }
}

func driftEvents(h *harness) []notify.Event {
	h.noti.mu.Lock()
	defer h.noti.mu.Unlock()
	var out []notify.Event
	for _, e := range h.noti.events {
		if e.Kind == notify.KindAgreementDrift {
			out = append(out, e)
		}
	}
	return out
}

// The reproduction: production wiring, agreement says 5%, supplier zero-rates.
// The alert must name the 5% the slots were checked against, not the unset 0.
func TestAgreementDriftReportsTheRateTheSlotsWereCheckedAgainst(t *testing.T) {
	f := zeroRatedFetcher(ts(t, "2026-10-22T23:00:00Z"), ts(t, "2026-10-23T23:00:00Z"))
	h := newProductionWiredHarness(t, ts(t, "2026-10-23T16:10:00Z"), f, constVAT(0.05))

	if _, err := h.c.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	evs := driftEvents(h)
	if len(evs) != 1 {
		t.Fatalf("want exactly one agreement_drift, got %d (%v)", len(evs), h.noti.kinds())
	}
	e := evs[0]
	if got, ok := e.Detail["configured_vat_rate"].(float64); !ok || got != 0.05 {
		t.Errorf("configured_vat_rate = %v, want 0.05 (the agreement's rate, not the unset legacy field)",
			e.Detail["configured_vat_rate"])
	}
	if !strings.Contains(e.Summary, "configuration says 5.00%") {
		t.Errorf("summary %q does not name the configured 5%%", e.Summary)
	}
	if strings.Contains(e.Summary, "0.00%, but configuration says 0.00%") {
		t.Errorf("summary contradicts itself: %q", e.Summary)
	}
}

// Same defect on the standing-charge side.
func TestStandingChargeDriftReportsTheRateTheChargesWereCheckedAgainst(t *testing.T) {
	base := newFakeFetcher(ts(t, "2026-10-01T00:00:00Z"), ts(t, "2026-10-23T23:00:00Z"))
	var charges []octopus.Rate
	day := ts(t, "2026-10-01T00:00:00Z")
	for i := 0; i < vatDriftMinSlots; i++ {
		from := day.AddDate(0, 0, i)
		to := from.AddDate(0, 0, 1)
		charges = append(charges, octopus.Rate{
			ValidFrom: from, ValidTo: &to,
			ExcVATPence: 56.19 + float64(i), IncVATPence: 56.19 + float64(i), // zero-rated
		})
	}
	f := scFetcher{fakeFetcher: base, charges: charges}
	h := newProductionWiredHarness(t, ts(t, "2026-10-23T16:10:00Z"), f, constVAT(0.05))

	if _, err := h.c.SyncStandingCharges(context.Background()); err != nil {
		t.Fatal(err)
	}
	evs := driftEvents(h)
	if len(evs) != 1 {
		t.Fatalf("want exactly one agreement_drift, got %d (%v)", len(evs), h.noti.kinds())
	}
	e := evs[0]
	if got, ok := e.Detail["configured_vat_rate"].(float64); !ok || got != 0.05 {
		t.Errorf("configured_vat_rate = %v, want 0.05", e.Detail["configured_vat_rate"])
	}
	if !strings.Contains(e.Summary, "configuration says 5.00%") {
		t.Errorf("summary %q does not name the configured 5%%", e.Summary)
	}
}

// Supplier rounding can push every slot past the 1e-6p per-slot tolerance while
// still implying the configured rate to within vatDriftEpsilon. That is rounding,
// not a rate change, and paging "5.00% vs 5.00%" for it is the same contradiction.
func TestAgreementDriftIgnoresRoundingAroundTheConfiguredRate(t *testing.T) {
	f := newFakeFetcher(ts(t, "2026-10-22T23:00:00Z"), ts(t, "2026-10-23T23:00:00Z"))
	f.priceAt = func(time.Time) (float64, float64) { return 20, 21.0001 } // implies 5.0005%
	h := newProductionWiredHarness(t, ts(t, "2026-10-23T16:10:00Z"), f, constVAT(0.05))

	res, err := h.c.Sync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Warnings) == 0 {
		t.Fatal("precondition: the per-slot check should still flag each slot")
	}
	if evs := driftEvents(h); len(evs) != 0 {
		t.Errorf("rounding around the configured rate raised drift: %q", evs[0].Summary)
	}
}

// A batch spanning two agreements with different rates has no single
// "configured" rate. The alert must say so rather than pick one.
func TestAgreementDriftAcrossAgreementsDoesNotClaimOneConfiguredRate(t *testing.T) {
	f := zeroRatedFetcher(ts(t, "2026-10-22T23:00:00Z"), ts(t, "2026-10-23T23:00:00Z"))
	cut := ts(t, "2026-10-23T11:00:00Z")
	vatAt := func(at time.Time) (float64, bool) {
		if at.Before(cut) {
			return 0.05, true
		}
		return 0.20, true
	}
	h := newProductionWiredHarness(t, ts(t, "2026-10-23T16:10:00Z"), f, vatAt)

	if _, err := h.c.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	evs := driftEvents(h)
	if len(evs) != 1 {
		t.Fatalf("want exactly one agreement_drift, got %d (%v)", len(evs), h.noti.kinds())
	}
	e := evs[0]
	if !strings.Contains(e.Summary, "5.00%") || !strings.Contains(e.Summary, "20.00%") {
		t.Errorf("summary %q should name both configured rates", e.Summary)
	}
	if _, ok := e.Detail["configured_vat_rate"]; ok {
		t.Errorf("configured_vat_rate set to %v for a batch with two configured rates",
			e.Detail["configured_vat_rate"])
	}
}

// The legacy scalar still works on its own: a collector given VATRate and no
// VATRateAt checks every slot against it, and the alert names it, because the
// warning carries whichever rate the check actually used. The other harnesses set
// both fields, so VATRateAt always wins and this path was otherwise untested.
func TestAgreementDriftNamesTheLegacyRateWhenItIsTheOneInUse(t *testing.T) {
	f := zeroRatedFetcher(ts(t, "2026-10-22T23:00:00Z"), ts(t, "2026-10-23T23:00:00Z"))
	store, err := prices.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { store.Close() }) //nolint:errcheck
	noti := &recordingNotifier{}
	c, err := New(Options{
		Fetcher: f, Store: store, Notifier: noti,
		Clock:    testutil.NewFakeClock(ts(t, "2026-10-23T16:10:00Z")),
		Location: london(t), TariffCode: testTariffCode,
		VATRate: 0.05, // and no VATRateAt
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := c.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	evs := driftEvents(&harness{noti: noti})
	if len(evs) != 1 {
		t.Fatalf("want exactly one agreement_drift, got %d (%v)", len(evs), noti.kinds())
	}
	if got, ok := evs[0].Detail["configured_vat_rate"].(float64); !ok || got != 0.05 {
		t.Errorf("configured_vat_rate = %v, want the legacy 0.05 in use", evs[0].Detail["configured_vat_rate"])
	}
}

// A ZERO legacy rate with no VATRateAt is "unset", not "0%": validation has no
// opinion and checks nothing, so a zero-rated supplier raises nothing. This is the
// half of #52 that Options.VATRate's doc used to get wrong ("taken literally — 0 is
// a legal VAT rate"), and the reason the alert must never read that field.
func TestZeroLegacyVATRateMeansNoCheck(t *testing.T) {
	f := zeroRatedFetcher(ts(t, "2026-10-22T23:00:00Z"), ts(t, "2026-10-23T23:00:00Z"))
	h := newProductionWiredHarness(t, ts(t, "2026-10-23T16:10:00Z"), f, nil)
	res, err := h.c.Sync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range res.Warnings {
		if w.Kind == prices.WarnVATMismatch {
			t.Fatalf("an unset VAT rate must not check anything, got %+v", w)
		}
	}
	if evs := driftEvents(h); len(evs) != 0 {
		t.Errorf("no configured rate, yet drift raised: %q", evs[0].Summary)
	}
}

// #51's shape: ONE open-ended standing charge, restated in place to zero-rated.
// Six slots is a half-hour noise floor a single charge can never reach, so this
// alerted on nothing while the bill quietly changed. One charge is enough.
func TestOneZeroRatedStandingChargeRaisesAgreementDrift(t *testing.T) {
	base := newFakeFetcher(ts(t, "2026-10-01T00:00:00Z"), ts(t, "2026-10-23T23:00:00Z"))
	f := scFetcher{fakeFetcher: base, charges: []octopus.Rate{{
		ValidFrom: ts(t, "2024-09-30T23:00:00Z"), ExcVATPence: 59.1606, IncVATPence: 59.1606,
	}}}
	h := newProductionWiredHarness(t, ts(t, "2026-10-23T01:03:00Z"), f, constVAT(0.05))

	if _, err := h.c.SyncStandingCharges(context.Background()); err != nil {
		t.Fatal(err)
	}
	if evs := driftEvents(h); len(evs) != 1 {
		t.Fatalf("want one agreement_drift for a single zero-rated charge, got %d (%v)", len(evs), h.noti.kinds())
	}
}

// And once the split is authored, the same charge is right: it is judged at the
// VAT in force when it was fetched, not at its 2024 start, so it does not alert
// on every daily sync from then on.
func TestARestatedStandingChargeIsQuietOnceTheSplitIsAuthored(t *testing.T) {
	base := newFakeFetcher(ts(t, "2026-10-01T00:00:00Z"), ts(t, "2026-10-23T23:00:00Z"))
	f := scFetcher{fakeFetcher: base, charges: []octopus.Rate{{
		ValidFrom: ts(t, "2024-09-30T23:00:00Z"), ExcVATPence: 59.1606, IncVATPence: 59.1606,
	}}}
	cut := ts(t, "2026-09-30T23:00:00Z")
	split := func(at time.Time) (float64, bool) {
		if at.Before(cut) {
			return 0.05, true
		}
		return 0, true
	}
	h := newProductionWiredHarness(t, ts(t, "2026-10-23T01:03:00Z"), f, split)

	if _, err := h.c.SyncStandingCharges(context.Background()); err != nil {
		t.Fatal(err)
	}
	if evs := driftEvents(h); len(evs) != 0 {
		t.Errorf("want no agreement_drift once config agrees with the supplier, got %v", evs)
	}
}
