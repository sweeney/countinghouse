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
