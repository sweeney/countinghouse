package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/sweeney/countinghouse/internal/config"
	"github.com/sweeney/countinghouse/internal/prices"
)

// ---------------------------------------------------------------------------
// Issue #36 idea 3: reach the archive by tariff code, and find out what is in it.
// ---------------------------------------------------------------------------

// coverageReader is a fakePriceReader that can also report coverage, so the
// optional-interface path is exercised as well as the absent one.
type coverageReader struct {
	fakePriceReader
	held []prices.TariffCoverage
	err  error
}

func (c coverageReader) Coverage(context.Context) ([]prices.TariffCoverage, error) {
	return c.held, c.err
}

type archivedTariffS struct {
	TariffCode    string `json:"tariff_code"`
	PaymentMethod string `json:"payment_method"`
	Slots         int    `json:"slots"`
	Complete      bool   `json:"complete"`
	Collected     bool   `json:"collected"`
	FirstSlot     string `json:"first_slot"`
	KnownTo       string `json:"known_to"`
}

func getArchivedTariffs(t *testing.T, s *Server) ([]archivedTariffS, int, string) {
	t.Helper()
	w := doGET(t, s, "/prices/tariffs")
	var body struct {
		Tariffs []archivedTariffS `json:"tariffs"`
	}
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode %q: %v", w.Body.String(), err)
		}
	}
	return body.Tariffs, w.Code, w.Body.String()
}

func TestArchivedTariffs_ReportsWhatIsHeld(t *testing.T) {
	s, _ := dataSetup(t)
	s.Clock = fixedClock{pxNow(t)}
	first := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	s.PriceReader = coverageReader{held: []prices.TariffCoverage{{
		TariffCode: "E-1R-AGILE-X", FirstSlot: first,
		KnownTo: first.Add(48 * prices.SlotLength), Slots: 48,
	}}}

	got, code, body := getArchivedTariffs(t, s)
	if code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", code, body)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 tariff, got %d: %s", len(got), body)
	}
	if got[0].TariffCode != "E-1R-AGILE-X" || got[0].Slots != 48 {
		t.Errorf("unexpected coverage: %+v", got[0])
	}
	// 48 slots across a 48-slot span is contiguous.
	if !got[0].Complete {
		t.Error("complete = false, but the rows fill their span")
	}
	// No collector is wired, so nothing is being synced — which is the state a
	// consumer polling for tomorrow's prices needs to see.
	if got[0].Collected {
		t.Error("collected = true with no collector wired")
	}
}

// The bounds alone cannot show an interior hole, which is why slots is reported
// beside them and complete is computed rather than assumed.
func TestArchivedTariffs_DetectsAnInteriorHole(t *testing.T) {
	s, _ := dataSetup(t)
	s.Clock = fixedClock{pxNow(t)}
	first := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	s.PriceReader = coverageReader{held: []prices.TariffCoverage{{
		TariffCode: "E-1R-AGILE-X", FirstSlot: first,
		KnownTo: first.Add(48 * prices.SlotLength), Slots: 40, // eight missing
	}}}

	got, _, _ := getArchivedTariffs(t, s)
	if got[0].Complete {
		t.Error("complete = true, but 40 rows cannot fill a 48-slot span")
	}
}

// An archive that cannot report coverage refuses clearly rather than pretending
// it holds nothing.
func TestArchivedTariffs_RefusesWithoutACoverageReader(t *testing.T) {
	s, _ := dataSetup(t)
	s.Clock = fixedClock{pxNow(t)}
	s.PriceReader = fakePriceReader{}

	if _, code, _ := getArchivedTariffs(t, s); code != http.StatusServiceUnavailable {
		t.Errorf("want 503 when coverage is unavailable, got %d", code)
	}
}

func TestArchivedTariffs_RefusesWithoutAnArchive(t *testing.T) {
	s, _ := dataSetup(t)
	s.Clock = fixedClock{pxNow(t)}
	s.PriceReader = nil

	if _, code, _ := getArchivedTariffs(t, s); code != http.StatusServiceUnavailable {
		t.Errorf("want 503 with no archive, got %d", code)
	}
}

// ---------------------------------------------------------------------------
// ?tariff_code= — the product, not the agreement.
// ---------------------------------------------------------------------------

type byCodeS struct {
	TariffCode string `json:"tariff_code"`
	Scope      string `json:"scope"`
	VATSource  string `json:"vat_source"`
	Complete   bool   `json:"complete"`
	Slots      []struct {
		ValidFrom   string  `json:"valid_from"`
		Price       float64 `json:"price"`
		PriceExcVAT float64 `json:"price_exc_vat"`
	} `json:"slots"`
	Missing []struct {
		From string `json:"from"`
		To   string `json:"to"`
	} `json:"missing"`
}

func getByCode(t *testing.T, s *Server, path string) (byCodeS, int, string) {
	t.Helper()
	w := doGET(t, s, path)
	var out byCodeS
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode %q: %v", w.Body.String(), err)
		}
	}
	return out, w.Code, w.Body.String()
}

// A flat-tariff window is where the agreement path gives flat_price and no
// slots. By code it gives the product's curve, because the product's curve
// exists regardless of what this site was buying.
func TestPricesByCode_AnswersUnderAFlatAgreement(t *testing.T) {
	s := pxFlatConfig(t)
	now := pxNow(t)
	s.PriceReader = fakePriceReader{slots: map[string][]prices.Slot{
		"E-1R-OTHER": pxSlots(now.Add(-2*time.Hour), 11, 12, 13, 14),
	}}

	got, code, body := getByCode(t, s,
		"/prices?window=today&tariff_code=E-1R-OTHER")
	if code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", code, body)
	}
	if got.Scope != "product" {
		t.Errorf("scope = %q, want product", got.Scope)
	}
	if len(got.Slots) == 0 {
		t.Fatalf("no slots for a code the archive holds: %s", body)
	}
	// The supplier's own figures, both bases, not ours grossed up.
	if got.VATSource != "supplier" {
		t.Errorf("vat_source = %q, want supplier", got.VATSource)
	}
	if got.Slots[0].PriceExcVAT == 0 {
		t.Error("the ex-VAT basis should be served alongside")
	}
}

// A window the archive does not reach must not arrive as an empty slots[]:
// "we don't have this" and "there were no prices" lead to opposite actions.
func TestPricesByCode_NamesWhatItDoesNotHold(t *testing.T) {
	s := pxFlatConfig(t)
	s.PriceReader = fakePriceReader{slots: map[string][]prices.Slot{}}

	got, code, body := getByCode(t, s, "/prices?window=today&tariff_code=E-1R-NEVER-HELD")
	if code != http.StatusOK {
		t.Fatalf("want 200 (a held-nothing answer is still an answer), got %d: %s", code, body)
	}
	if got.Complete {
		t.Error("complete = true for a code the archive has never held")
	}
	if len(got.Missing) == 0 {
		t.Fatalf("an unreachable window must say what is missing: %s", body)
	}
	// Collapsed into ranges rather than one row per absent half hour.
	if len(got.Missing) > 2 {
		t.Errorf("missing should collapse into contiguous ranges, got %d", len(got.Missing))
	}
}

// Agreements are irrelevant to a question about the product, so there is no
// switchover boundary to refuse at — the agreement path's 400 must not apply.
func TestPricesByCode_DoesNotRefuseASwitchoverWindow(t *testing.T) {
	s, _ := dataSetup(t)
	s.Clock = fixedClock{pxNow(t)}
	now := pxNow(t)
	// Two agreements either side of a boundary inside the window. The first
	// starts well before the window so the whole span is covered — otherwise the
	// agreement path refuses for the uncovered stretch rather than the switchover,
	// and this test would prove the wrong thing.
	early := now.Add(-90 * 24 * time.Hour)
	mid := now.Add(-24 * time.Hour)
	s.Config = pxConfig{agreements: config.EnergyAgreements{
		Agreements: map[string][]config.Agreement{"electricity": {
			{From: &early, To: &mid, Name: "Old", Type: config.TariffTypeFixed,
				VATRate: 0.05, UnitRate: 0.20, DailyStandingCharge: 0.5},
			{From: &mid, Name: "New", Type: config.TariffTypeFixed,
				VATRate: 0.05, UnitRate: 0.25, DailyStandingCharge: 0.5},
		}},
	}}
	s.PriceReader = fakePriceReader{slots: map[string][]prices.Slot{
		"E-1R-OTHER": pxSlots(now.Add(-2*time.Hour), 11, 12),
	}}

	// The agreement path refuses this window...
	if w := doGET(t, s, "/prices?window=7d"); w.Code != http.StatusBadRequest {
		t.Fatalf("fixture should span a tariff change; agreement path gave %d: %s",
			w.Code, w.Body.String())
	}
	// ...and the product path does not.
	if _, code, body := getByCode(t, s, "/prices?window=7d&tariff_code=E-1R-OTHER"); code != http.StatusOK {
		t.Errorf("by code should ignore agreement boundaries, got %d: %s", code, body)
	}
}

func TestPricesByCode_RefusesWithoutAnArchive(t *testing.T) {
	s := pxFlatConfig(t)
	s.PriceReader = nil
	if _, code, _ := getByCode(t, s, "/prices?window=today&tariff_code=E-1R-X"); code != http.StatusServiceUnavailable {
		t.Errorf("want 503 with no archive, got %d", code)
	}
}

// The day cap still applies: an unbounded window is an unbounded response
// whichever question it asks.
func TestPricesByCode_StillCapsTheWindow(t *testing.T) {
	s := pxFlatConfig(t)
	_, code, _ := getByCode(t, s,
		"/prices?window=custom&from=2026-01-01T00:00:00Z&to=2026-06-01T00:00:00Z&tariff_code=E-1R-X")
	if code != http.StatusBadRequest {
		t.Errorf("want 400 over the cap, got %d", code)
	}
}

// ---------------------------------------------------------------------------
// /prices/stats?tariff_code= — the other half of friction #2.
//
// /prices gained ?tariff_code= so the archive could be READ by code. Without the
// same here the archive could not be AGGREGATED by code, so "how often do cheap
// slots occur in winter versus summer" was still ~24 paginated /prices calls and
// a client-side rollup: the rollup existed but could not be pointed at it.
// ---------------------------------------------------------------------------

// The reviewer's repro: a seasonal window, by code, far outside any configured
// agreement. The agreement path refuses it; the product path answers.
func TestArchivedStats_AnswersASeasonalWindowByCode(t *testing.T) {
	s := pxFlatConfig(t)
	now := pxNow(t)
	// Two days of slots inside the window we ask about.
	start := now.Add(-48 * time.Hour)
	s.PriceReader = fakePriceReader{slots: map[string][]prices.Slot{
		"E-1R-AGILE-X": pxSlots(start, 10, 20, 5, 40, 30, 60),
	}}

	w := doGET(t, s, "/prices/stats?window=7d&group_by=month&tariff_code=E-1R-AGILE-X&cheap_below=10")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	var body struct {
		TariffCode string  `json:"tariff_code"`
		Scope      string  `json:"scope"`
		VATSource  string  `json:"vat_source"`
		GroupBy    string  `json:"group_by"`
		CheapBelow float64 `json:"cheap_below"`
		Periods    []struct {
			Period     string  `json:"period"`
			Slots      int     `json:"slots"`
			CheapSlots *int    `json:"cheap_slots"`
			Min        float64 `json:"min"`
		} `json:"periods"`
		Days []any `json:"days"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Scope != "product" || body.TariffCode != "E-1R-AGILE-X" {
		t.Errorf("scope/code = %q/%q, want product/E-1R-AGILE-X", body.Scope, body.TariffCode)
	}
	// The supplier's own VAT, as on the by-code curve.
	if body.VATSource != "supplier" {
		t.Errorf("vat_source = %q, want supplier", body.VATSource)
	}
	if len(body.Periods) == 0 {
		t.Fatalf("no periods: %s", w.Body.String())
	}
	if body.Periods[0].CheapSlots == nil {
		t.Error("cheap_below was supplied but no cheap counts came back")
	}
	// periods[] at every grouping on this route; never the agreement path's
	// days[], whose per-day schema is different.
	if body.Days != nil {
		t.Error("days[] must not appear on the by-code route")
	}
}

// Agreement boundaries are irrelevant to a question about the product, so the
// window the agreement path refuses is answerable here. This is the gap the
// review reported, stated as a test.
func TestArchivedStats_IgnoresAgreementBoundaries(t *testing.T) {
	s, _ := dataSetup(t)
	s.Clock = fixedClock{pxNow(t)}
	now := pxNow(t)
	early := now.Add(-90 * 24 * time.Hour)
	mid := now.Add(-24 * time.Hour)
	s.Config = pxConfig{agreements: config.EnergyAgreements{
		Agreements: map[string][]config.Agreement{"electricity": {
			{From: &early, To: &mid, Name: "Old", Type: config.TariffTypeFixed,
				VATRate: 0.05, UnitRate: 0.20, DailyStandingCharge: 0.5},
			{From: &mid, Name: "New", Type: config.TariffTypeFixed,
				VATRate: 0.05, UnitRate: 0.25, DailyStandingCharge: 0.5},
		}},
	}}
	s.PriceReader = fakePriceReader{slots: map[string][]prices.Slot{
		"E-1R-AGILE-X": pxSlots(now.Add(-2*time.Hour), 11, 12),
	}}

	// The agreement path refuses this window...
	if w := doGET(t, s, "/prices/stats?window=7d&group_by=month"); w.Code == http.StatusOK {
		t.Fatalf("fixture should span a tariff change; agreement path gave 200")
	}
	// ...and the product path answers it.
	w := doGET(t, s, "/prices/stats?window=7d&group_by=month&tariff_code=E-1R-AGILE-X")
	if w.Code != http.StatusOK {
		t.Errorf("by code should ignore agreement boundaries, got %d: %s", w.Code, w.Body.String())
	}
}

// A monthly row silently computed over a partial month is exactly the failure
// `days` was added to prevent, so an unreachable stretch is reported.
func TestArchivedStats_ReportsWhatItCannotReach(t *testing.T) {
	s := pxFlatConfig(t)
	s.PriceReader = fakePriceReader{slots: map[string][]prices.Slot{}}

	w := doGET(t, s, "/prices/stats?window=7d&group_by=month&tariff_code=E-1R-NEVER-HELD")
	if w.Code != http.StatusOK {
		t.Fatalf("a held-nothing answer is still an answer, got %d: %s", w.Code, w.Body.String())
	}
	var body struct {
		Complete bool `json:"complete"`
		Missing  []struct {
			From string `json:"from"`
		} `json:"missing"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Complete {
		t.Error("complete = true for a code the archive has never held")
	}
	if len(body.Missing) == 0 {
		t.Errorf("an unreachable window must say what is missing: %s", w.Body.String())
	}
}

// The 366-day cap, not /prices' 31 — the reason a seasonal window belongs here.
func TestArchivedStats_UsesTheDailyRowCapNotTheSlotCap(t *testing.T) {
	s := pxFlatConfig(t)
	s.PriceReader = fakePriceReader{slots: map[string][]prices.Slot{}}

	// 90 days: over /prices' 31-day cap, well inside this route's 366.
	if w := doGET(t, s,
		"/prices/stats?window=custom&from=2026-01-01T00:00:00Z&to=2026-04-01T00:00:00Z&tariff_code=E-1R-X"); w.Code != http.StatusOK {
		t.Errorf("90 days should be inside the 366-day cap, got %d: %s", w.Code, w.Body.String())
	}
	// Over 366 still refuses.
	if w := doGET(t, s,
		"/prices/stats?window=custom&from=2024-01-01T00:00:00Z&to=2026-01-01T00:00:00Z&tariff_code=E-1R-X"); w.Code != http.StatusBadRequest {
		t.Errorf("two years should exceed the cap, got %d", w.Code)
	}
}

func TestArchivedStats_RefusesWithoutAnArchive(t *testing.T) {
	s := pxFlatConfig(t)
	s.PriceReader = nil
	if w := doGET(t, s, "/prices/stats?window=7d&tariff_code=E-1R-X"); w.Code != http.StatusServiceUnavailable {
		t.Errorf("want 503 with no archive, got %d", w.Code)
	}
}

// The agreement-scoped route is untouched by any of this.
func TestArchivedStats_AgreementPathUnchanged(t *testing.T) {
	now := pxNow(t)
	s := pxSetup(t, pxSlots(now.Add(-24*time.Hour), 20, 21, 22, -3, 60, 5))

	w := doGET(t, s, "/prices/stats?window=7d")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	var body struct {
		Days  []any `json:"days"`
		Scope any   `json:"scope"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Days) == 0 {
		t.Error("the agreement path must still serve days[]")
	}
	if body.Scope != nil {
		t.Error("scope is a by-code field and must not leak into the agreement path")
	}
}
