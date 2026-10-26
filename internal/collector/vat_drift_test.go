package collector

import (
	"context"
	"strings"
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

func vatWarning(implied, expected float64) prices.Warning {
	v := implied
	e := expected
	return prices.Warning{Kind: prices.WarnVATMismatch, ImpliedVATRate: &v, ExpectedVATRate: &e}
}

func TestVATDriftRequiresAConsistentRate(t *testing.T) {
	// Slots disagreeing in DIFFERENT directions are a data problem, not a tax
	// change. Paging for that would train somebody to ignore the page.
	mixed := []prices.Warning{
		vatWarning(0, 0.10), vatWarning(0.05, 0.10), vatWarning(0.20, 0.10),
		vatWarning(0, 0.10), vatWarning(0.13, 0.10), vatWarning(0.02, 0.10),
	}
	if _, ok := vatDrift(mixed); ok {
		t.Error("inconsistent implied rates must not be reported as drift")
	}
}

func TestVATDriftNeedsEnoughSlotsToBeCredible(t *testing.T) {
	var few []prices.Warning
	for i := 0; i < vatDriftMinSlots-1; i++ {
		few = append(few, vatWarning(0, 0.10))
	}
	if _, ok := vatDrift(few); ok {
		t.Errorf("%d slots is noise, not a rate change", len(few))
	}

	few = append(few, vatWarning(0, 0.10))
	if _, ok := vatDrift(few); !ok {
		t.Errorf("%d consistent slots should be reported", len(few))
	}
}

// Supplier figures are rounded pence, so two "identical" rates differ slightly.
// The tolerance has to absorb that or the alert never fires on real data.
func TestVATDriftToleratesRoundingInTheImpliedRate(t *testing.T) {
	var w []prices.Warning
	for i := 0; i < vatDriftMinSlots; i++ {
		w = append(w, vatWarning(0.0500+float64(i)*1e-5, 0.20))
	}
	if _, ok := vatDrift(w); !ok {
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
	if _, ok := vatDrift(w); ok {
		t.Error("warnings carrying no implied rate must not raise drift")
	}
}

// A warning carrying no expected rate cannot say what configuration disagrees
// WITH, so it cannot support an alert naming "what configuration says".
func TestVATDriftIgnoresWarningsWithNoExpectedRate(t *testing.T) {
	var w []prices.Warning
	for i := 0; i < vatDriftMinSlots; i++ {
		v := 0.0
		w = append(w, prices.Warning{Kind: prices.WarnVATMismatch, ImpliedVATRate: &v})
	}
	if _, ok := vatDrift(w); ok {
		t.Error("warnings carrying no expected rate must not raise drift")
	}
}

// The configured rate comes from the warnings, and an implied rate that matches
// it to within vatDriftEpsilon is rounding rather than a rate change (#52).
func TestVATDriftTakesConfiguredFromTheWarnings(t *testing.T) {
	var w []prices.Warning
	for i := 0; i < vatDriftMinSlots; i++ {
		w = append(w, vatWarning(0, 0.05))
	}
	d, ok := vatDrift(w)
	if !ok {
		t.Fatal("0% against a configured 5% is drift")
	}
	if d.ConfiguredMin != 0.05 || d.ConfiguredMax != 0.05 {
		t.Errorf("configured = [%v, %v], want 0.05", d.ConfiguredMin, d.ConfiguredMax)
	}

	var near []prices.Warning
	for i := 0; i < vatDriftMinSlots; i++ {
		near = append(near, vatWarning(0.05+1e-4, 0.05))
	}
	if d, ok := vatDrift(near); ok {
		t.Errorf("implied %v against configured %v is rounding, not drift", d.Implied, d.ConfiguredMin)
	}
}

// Whatever reaches describe, the two sides never print as the same figure.
func TestVATDriftDescribeNeverPrintsIdenticalFigures(t *testing.T) {
	cases := []vatDriftFinding{
		{Implied: 0, ConfiguredMin: 0.05, ConfiguredMax: 0.05},
		{Implied: 0.05, ConfiguredMin: 0.050001, ConfiguredMax: 0.050001},
		{Implied: 0.2, ConfiguredMin: 0.05, ConfiguredMax: 0.2000004},
	}
	for _, d := range cases {
		implied, configured := d.describe()
		if implied == configured || strings.Contains(configured, " "+implied+" ") {
			t.Errorf("%+v: implied %q printed identically to configured %q", d, implied, configured)
		}
	}
}
