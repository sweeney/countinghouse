package collector

import (
	"context"
	"testing"
	"time"

	"github.com/sweeney/countinghouse/internal/notify"
	"github.com/sweeney/countinghouse/internal/prices"
)

// ---------------------------------------------------------------------------
// A whole batch implying ONE consistent VAT rate that is not the configured one
// is a rate change, not a data blip — and a rate change is a statutory event
// somebody has to act on. The temporary zero rate on domestic electricity in
// Great Britain (1 Oct 2026 – 31 Mar 2027) is the case in hand.
//
// Per-slot warnings alone are not enough: they are counted, not paged, and a
// signal nobody reads is the failure mode this service already learned once.
// ---------------------------------------------------------------------------

// zeroRatedFetcher publishes prices where inc == exc: what the supplier will send
// once the zero rate is in force.
func zeroRatedFetcher(from, to time.Time) *fakeFetcher {
	f := newFakeFetcher(from, to)
	f.priceAt = func(time.Time) (float64, float64) { return 20, 20 }
	return f
}

// THE headline alert. Config says 5%, every slot implies 0%, and that is worth
// waking somebody for — once, with the number to put in config.
func TestConsistentVATMismatchAcrossABatchRaisesAgreementDrift(t *testing.T) {
	f := zeroRatedFetcher(ts(t, "2026-09-30T23:00:00Z"), ts(t, "2026-10-01T22:00:00Z"))
	h := newHarness(t, ts(t, "2026-10-01T16:10:00Z"), f)

	if _, err := h.c.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}

	if !h.noti.has(notify.KindAgreementDrift) {
		t.Fatalf("no agreement_drift raised; got %v", h.noti.kinds())
	}

	// The alert has to carry the actionable number, not just the complaint.
	var found bool
	for _, e := range h.noti.events {
		if e.Kind != notify.KindAgreementDrift {
			continue
		}
		found = true
		if got, ok := e.Detail["implied_vat_rate"].(float64); !ok || got != 0 {
			t.Errorf("implied_vat_rate = %v, want 0", e.Detail["implied_vat_rate"])
		}
		if got, ok := e.Detail["configured_vat_rate"].(float64); !ok || got != 0.05 {
			t.Errorf("configured_vat_rate = %v, want 0.05", e.Detail["configured_vat_rate"])
		}
	}
	if !found {
		t.Fatal("agreement_drift event not found in details")
	}
}

// The prices are still ARCHIVED. An alert about configuration must never be
// confused with a reason to stop collecting.
func TestAgreementDriftDoesNotStopTheArchiveFilling(t *testing.T) {
	f := zeroRatedFetcher(ts(t, "2026-09-30T23:00:00Z"), ts(t, "2026-10-01T22:00:00Z"))
	h := newHarness(t, ts(t, "2026-10-01T16:10:00Z"), f)

	res, err := h.c.Sync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Stored.Inserted == 0 {
		t.Fatal("nothing was stored during a VAT disagreement")
	}
	if len(res.Rejected) != 0 {
		t.Errorf("%d slots rejected; the VAT check is Gate B", len(res.Rejected))
	}
}

// ---------------------------------------------------------------------------
// vatDrift itself, where the judgement lives
// ---------------------------------------------------------------------------

func vatWarning(implied float64) prices.Warning {
	v := implied
	return prices.Warning{Kind: prices.WarnVATMismatch, ImpliedVATRate: &v}
}

func TestVATDriftRequiresAConsistentRate(t *testing.T) {
	// Slots disagreeing in DIFFERENT directions are a data problem, not a tax
	// change. Paging for that would train somebody to ignore the page.
	mixed := []prices.Warning{
		vatWarning(0), vatWarning(0.05), vatWarning(0.20),
		vatWarning(0), vatWarning(0.13), vatWarning(0.02),
	}
	if _, ok := vatDrift(mixed, 0.05); ok {
		t.Error("inconsistent implied rates must not be reported as drift")
	}
}

func TestVATDriftNeedsEnoughSlotsToBeCredible(t *testing.T) {
	var few []prices.Warning
	for i := 0; i < vatDriftMinSlots-1; i++ {
		few = append(few, vatWarning(0))
	}
	if _, ok := vatDrift(few, 0.05); ok {
		t.Errorf("%d slots is noise, not a rate change", len(few))
	}

	few = append(few, vatWarning(0))
	if _, ok := vatDrift(few, 0.05); !ok {
		t.Errorf("%d consistent slots should be reported", len(few))
	}
}

// Supplier figures are rounded pence, so two "identical" rates differ slightly.
// The tolerance has to absorb that or the alert never fires on real data.
func TestVATDriftToleratesRoundingInTheImpliedRate(t *testing.T) {
	var w []prices.Warning
	for i := 0; i < vatDriftMinSlots; i++ {
		w = append(w, vatWarning(0.0500+float64(i)*1e-5))
	}
	if _, ok := vatDrift(w, 0.20); !ok {
		t.Error("rates differing by rounding noise should count as one rate")
	}
}

// A warning with no implied rate (exc was zero, so the ratio is undefined) is not
// evidence of anything and must not be counted toward the threshold.
func TestVATDriftIgnoresWarningsWithNoImpliedRate(t *testing.T) {
	var w []prices.Warning
	for i := 0; i < vatDriftMinSlots; i++ {
		w = append(w, prices.Warning{Kind: prices.WarnVATMismatch})
	}
	if _, ok := vatDrift(w, 0.05); ok {
		t.Error("warnings carrying no implied rate must not raise drift")
	}
}
