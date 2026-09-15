package httpapi

import (
	"net/http"
	"testing"
	"time"

	"github.com/sweeney/countinghouse/internal/config"
	"github.com/sweeney/countinghouse/internal/prices"
)

// ---------------------------------------------------------------------------
// The temporary zero rate of VAT on domestic electricity in Great Britain:
// 0% for supplies from 1 October 2026 to 31 March 2027, 5% either side.
//
// Expressing that means splitting the agreement into dated blocks — which is what
// the dated-agreements document is for. But the blocks differ only in VAT: the
// TARIFF CODE is identical throughout, because the tariff did not change, the tax
// did. The price routes must not mistake a tax change for a tariff change.
// ---------------------------------------------------------------------------

// vatSplitAgreements is one Agile tariff across the zero-rate transition: same
// code either side, different VAT rate.
func vatSplitAgreements(t *testing.T, loc *time.Location) config.EnergyAgreements {
	t.Helper()
	began := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	starts := time.Date(2026, 10, 1, 0, 0, 0, 0, loc)
	ends := time.Date(2027, 4, 1, 0, 0, 0, 0, loc)

	return config.EnergyAgreements{Agreements: map[string][]config.Agreement{
		"electricity": {
			{From: &began, To: &starts, Name: "Agile", Type: config.TariffTypeVariable,
				ID: pxTariff, Unit: "kWh", VATRate: 0.05, DailyStandingCharge: 0.59},
			{From: &starts, To: &ends, Name: "Agile (VAT zero-rated)", Type: config.TariffTypeVariable,
				ID: pxTariff, Unit: "kWh", VATRate: 0, DailyStandingCharge: 0.59},
			{From: &ends, Name: "Agile", Type: config.TariffTypeVariable,
				ID: pxTariff, Unit: "kWh", VATRate: 0.05, DailyStandingCharge: 0.59},
		},
	}}
}

// vatSetup wires a server sitting just after the zero rate took effect, holding
// prices either side of it.
func vatSetup(t *testing.T) *Server {
	t.Helper()
	s, _ := dataSetup(t)
	loc := s.loc()

	// 00:00 on 30 September through 23:30 on 2 October: four days of slots
	// straddling the transition.
	start := time.Date(2026, 9, 30, 0, 0, 0, 0, loc).UTC()
	rates := make([]float64, 0, 48*3)
	for i := 0; i < 48*3; i++ {
		rates = append(rates, 20+float64(i%12))
	}

	s.Clock = fixedClock{time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)}
	s.Config = pxConfig{agreements: vatSplitAgreements(t, loc)}
	s.PriceReader = fakePriceReader{slots: map[string][]prices.Slot{
		pxTariff: pxSlots(start, rates...),
	}}
	return s
}

// THE defect. The boundary refusal exists because a curve belongs to one tariff —
// but here it IS one tariff, and the window is perfectly coherent. Refusing it
// would mean that for the six months of the zero rate, and again for six months
// after it ends, no consumer could ask for a month of prices spanning the change.
func TestPriceCurveIsServedAcrossAVATOnlyAgreementSplit(t *testing.T) {
	s := vatSetup(t)

	w := doGET(t, s, "/prices?window=custom"+
		"&from=2026-09-30T00:00:00Z&to=2026-10-02T00:00:00Z")

	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 — the tariff code is identical either side of a "+
			"VAT change, so there is exactly one curve: %s", w.Code, w.Body.String())
	}
	m := decode(t, w)
	if m["tariff_code"] != pxTariff {
		t.Errorf("tariff_code = %v, want %q", m["tariff_code"], pxTariff)
	}
	slots, ok := m["slots"].([]any)
	if !ok || len(slots) == 0 {
		t.Fatalf("no slots returned across the boundary: %v", m["slots"])
	}
}

// /prices/stats spans the change the same way — a month of daily spreads is
// exactly the request somebody makes to ask "did the zero rate help?".
func TestPriceStatsAreServedAcrossAVATOnlyAgreementSplit(t *testing.T) {
	s := vatSetup(t)

	w := doGET(t, s, "/prices/stats?window=custom"+
		"&from=2026-09-30T00:00:00Z&to=2026-10-02T00:00:00Z")

	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", w.Code, w.Body.String())
	}
	days, ok := decode(t, w)["days"].([]any)
	if !ok || len(days) < 2 {
		t.Fatalf("want a row either side of the transition, got %v", days)
	}
}

// The refusal must still fire when the tariff GENUINELY changes, because then the
// slots really do come from two different curves and one `tariff_code` on the
// response would be a lie. This is the test that stops the fix above from being a
// blanket removal.
func TestPriceCurveStillRefusesAGenuineTariffChange(t *testing.T) {
	s, _ := dataSetup(t)
	loc := s.loc()
	began := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	switched := time.Date(2026, 10, 1, 0, 0, 0, 0, loc)

	s.Clock = fixedClock{time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)}
	s.Config = pxConfig{agreements: config.EnergyAgreements{
		Agreements: map[string][]config.Agreement{"electricity": {
			{From: &began, To: &switched, Name: "Agile", Type: config.TariffTypeVariable,
				ID: pxTariff, Unit: "kWh", VATRate: 0.05, DailyStandingCharge: 0.59},
			{From: &switched, Name: "Go", Type: config.TariffTypeVariable,
				ID: "E-1R-GO-24-10-01-A", Unit: "kWh", VATRate: 0.05, DailyStandingCharge: 0.61},
		}},
	}}
	s.PriceReader = fakePriceReader{slots: map[string][]prices.Slot{}}

	w := doGET(t, s, "/prices?window=custom"+
		"&from=2026-09-30T00:00:00Z&to=2026-10-02T00:00:00Z")

	if w.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400 — two different tariff codes cannot share one curve", w.Code)
	}
}

// A FLAT tariff across a VAT change is the case where refusal is still right even
// though the code is unchanged: `flat_price` is a single inc-VAT number derived
// from config, and it genuinely differs either side. One field cannot carry both.
func TestFlatTariffStillRefusesAcrossAVATChange(t *testing.T) {
	s, _ := dataSetup(t)
	loc := s.loc()
	began := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	starts := time.Date(2026, 10, 1, 0, 0, 0, 0, loc)

	s.Clock = fixedClock{time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)}
	s.Config = pxConfig{agreements: config.EnergyAgreements{
		Agreements: map[string][]config.Agreement{"electricity": {
			{From: &began, To: &starts, Name: "Fixed", Type: config.TariffTypeFixed,
				Unit: "kWh", UnitRate: 0.25, VATRate: 0.05, DailyStandingCharge: 0.59},
			{From: &starts, Name: "Fixed (VAT zero-rated)", Type: config.TariffTypeFixed,
				Unit: "kWh", UnitRate: 0.25, VATRate: 0, DailyStandingCharge: 0.59},
		}},
	}}

	w := doGET(t, s, "/prices?window=custom"+
		"&from=2026-09-30T00:00:00Z&to=2026-10-02T00:00:00Z")

	if w.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400 — 26.25p and 25.00p cannot both be flat_price: %s",
			w.Code, w.Body.String())
	}
}
