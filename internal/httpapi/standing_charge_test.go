package httpapi

import (
	"context"
	"testing"
	"time"

	"github.com/sweeney/countinghouse/internal/prices"
)

// ---------------------------------------------------------------------------
// The standing charge was the LAST number in a bill still grossed up from
// configuration, and therefore the last one a stale vat_rate could silently get
// wrong. Archiving the supplier's own figure demotes config's vat_rate to a
// checkable expectation everywhere.
// ---------------------------------------------------------------------------

// scReader is a PriceReader that also serves standing charges.
type scReader struct {
	fakePriceReader
	charges []prices.DailyCharge
	err     error
}

func (r scReader) StandingCharges(_ context.Context, code string, from, to time.Time) ([]prices.DailyCharge, error) {
	if r.err != nil {
		return nil, r.err
	}
	var out []prices.DailyCharge
	for _, c := range r.charges {
		end := to
		if c.ValidTo != nil {
			end = *c.ValidTo
		}
		if c.TariffCode == code && c.ValidFrom.Before(to) && (c.ValidTo == nil || end.After(from)) {
			out = append(out, c)
		}
	}
	return out, nil
}

func scCharge(from time.Time, to *time.Time, exc, inc float64) prices.DailyCharge {
	return prices.DailyCharge{
		TariffCode: pxTariff, ValidFrom: from, ValidTo: to,
		ExcVATPence: exc, IncVATPence: inc, RetrievedAt: from,
	}
}

// THE case this was built for: the supplier has zero-rated the standing charge
// and the agreement says so too. The bill must use the supplier's figure, not the
// configured daily_standing_charge (0.59 ex VAT), which is a different number.
func TestStandingChargeComesFromTheArchiveNotConfig(t *testing.T) {
	s := vatSetup(t) // config: 5% before 1 Oct, 0% after; clock 2 Oct
	base := s.PriceReader.(fakePriceReader)

	// One open-ended zero-rated charge covering the whole window.
	s.PriceReader = scReader{
		fakePriceReader: base,
		charges: []prices.DailyCharge{
			scCharge(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), nil, 56.19, 56.19),
		},
	}

	// 00:00 BST on 1 Oct to 01:00 BST on 2 Oct: 25 hours, all under the zero rate.
	w := doGET(t, s, "/bill?window=custom&from=2026-09-30T23:00:00Z&to=2026-10-02T00:00:00Z")
	if w.Code != 200 {
		t.Fatalf("got %d: %s", w.Code, w.Body.String())
	}
	m := decode(t, w)

	if m["standing_charge_source"] != "archive" {
		t.Errorf("standing_charge_source = %v, want archive", m["standing_charge_source"])
	}
	got := m["standing_charge"].(float64)
	if want := 25.0 / 24 * 0.5619; got < want-0.0001 || got > want+0.0001 {
		t.Errorf("standing_charge £%.4f, want £%.4f (the supplier's own inc-VAT figure)", got, want)
	}
}

// #51, as it happened in production. The supplier published ONE standing charge,
// open-ended from 2024, and on 1 Oct 2026 restated it in place to the zero-rated
// figure (inc == exc). Billing that inc-VAT figure for a day before the cut bills
// a 5%-VAT day without VAT. The ex-VAT figure is still right; only the VAT in the
// inc column is backdated, so the day is charged at the ex-VAT figure grossed up
// by the agreement covering it.
func TestBackdatedZeroRatedStandingChargeKeepsVATBeforeTheCut(t *testing.T) {
	s := vatSetup(t)
	base := s.PriceReader.(fakePriceReader)
	s.PriceReader = scReader{
		fakePriceReader: base,
		charges: []prices.DailyCharge{
			scCharge(time.Date(2024, 9, 30, 23, 0, 0, 0, time.UTC), nil, 59.1606, 59.1606),
		},
	}

	// 21 Sep 2026, a whole local day under the 5% agreement.
	w := doGET(t, s, "/bill?window=custom&from=2026-09-20T23:00:00Z&to=2026-09-21T23:00:00Z")
	if w.Code != 200 {
		t.Fatalf("got %d: %s", w.Code, w.Body.String())
	}
	m := decode(t, w)
	got := m["standing_charge"].(float64)
	if want := 0.591606 * 1.05; got < want-0.0001 || got > want+0.0001 {
		t.Errorf("standing_charge £%.4f, want £%.4f: 59.1606p ex VAT at the 5%% the agreement "+
			"for that day says, not the zero-rated inc figure backdated to 2024", got, want)
	}
	if m["standing_charge_source"] != "archive_ex_vat" {
		t.Errorf("standing_charge_source = %v, want archive_ex_vat: the bill used the "+
			"supplier's ex-VAT figure, not its inc-VAT one, and must say so", m["standing_charge_source"])
	}
}

// The same backdated record across the cut: each side gets its own agreement's
// VAT. The zero-rated side agrees with the supplier and bills its figure as-is.
func TestBackdatedStandingChargeAcrossTheCutBillsEachSideAtItsOwnVAT(t *testing.T) {
	s := vatSetup(t)
	base := s.PriceReader.(fakePriceReader)
	s.PriceReader = scReader{
		fakePriceReader: base,
		charges: []prices.DailyCharge{
			scCharge(time.Date(2024, 9, 30, 23, 0, 0, 0, time.UTC), nil, 56.19, 56.19),
		},
	}

	// 00:00Z 30 Sep to 00:00Z 2 Oct. The agreements split at local midnight,
	// 23:00Z: 23 hours at 5%, then 25 at 0%.
	m := decode(t, doGET(t, s, "/bill?window=custom&from=2026-09-30T00:00:00Z&to=2026-10-02T00:00:00Z"))
	got := m["standing_charge"].(float64)
	want := 23.0/24*0.5619*1.05 + 25.0/24*0.5619
	if got < want-0.0001 || got > want+0.0001 {
		t.Errorf("standing_charge £%.4f, want £%.4f (23h at 5%%, 25h at 0%%)", got, want)
	}
	if m["standing_charge_source"] != "archive_ex_vat" {
		t.Errorf("standing_charge_source = %v, want archive_ex_vat", m["standing_charge_source"])
	}
}

// A supplier inc-VAT figure that rounds the agreement's rate is the agreement's
// rate: it is billed as-is, not recomputed. Same tolerance as the drift alert.
func TestStandingChargeWithinRoundingOfTheAgreementIsBilledAsIs(t *testing.T) {
	s := vatSetup(t)
	base := s.PriceReader.(fakePriceReader)
	s.PriceReader = scReader{
		fakePriceReader: base,
		charges: []prices.DailyCharge{
			// 59.1606 x 1.05 = 62.11863; the supplier rounds to 62.1186.
			scCharge(time.Date(2024, 9, 30, 23, 0, 0, 0, time.UTC), nil, 59.1606, 62.1186),
		},
	}

	m := decode(t, doGET(t, s, "/bill?window=custom&from=2026-09-20T23:00:00Z&to=2026-09-21T23:00:00Z"))
	if m["standing_charge_source"] != "archive" {
		t.Errorf("standing_charge_source = %v, want archive: rounding is not disagreement",
			m["standing_charge_source"])
	}
	// The bill rounds money to 4dp, which cannot tell 62.1186p from 62.11863p;
	// the source above is what proves the supplier's figure was used.
	got := m["standing_charge"].(float64)
	if want := 0.621186; got < want-0.0001 || got > want+0.0001 {
		t.Errorf("standing_charge £%.4f, want the supplier's own £%.4f", got, want)
	}
}

// A window the archive only partly covers falls back to config WHOLE, rather
// than billing the covered days and quietly dropping the rest.
func TestPartialArchiveCoverageFallsBackToConfigEntirely(t *testing.T) {
	s := vatSetup(t)
	base := s.PriceReader.(fakePriceReader)

	// Charge starts a day INTO the window, leaving the first day uncovered.
	s.PriceReader = scReader{
		fakePriceReader: base,
		charges: []prices.DailyCharge{
			scCharge(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), nil, 56.19, 56.19),
		},
	}

	m := decode(t, doGET(t, s, "/bill?window=custom&from=2026-09-30T00:00:00Z&to=2026-10-02T00:00:00Z"))
	if m["standing_charge_source"] != "config" {
		t.Errorf("source = %v, want config — a partly covered window must not be part-billed",
			m["standing_charge_source"])
	}
}

// No archived standing charges at all: every instance behaved this way before
// this existed, and must continue to.
func TestNoArchivedStandingChargesFallsBackToConfig(t *testing.T) {
	s := vatSetup(t)
	m := decode(t, doGET(t, s, "/bill?window=custom&from=2026-09-30T00:00:00Z&to=2026-10-02T00:00:00Z"))

	if m["standing_charge_source"] != "config" {
		t.Errorf("source = %v, want config", m["standing_charge_source"])
	}
	if _, ok := m["standing_charge"]; !ok {
		t.Error("a bill must still carry a standing charge with no archive")
	}
}

// A read failure must not fail the bill. The archive is an improvement on the
// configured figure, not a dependency of it.
func TestStandingChargeReadFailureFallsBackRatherThanFailing(t *testing.T) {
	s := vatSetup(t)
	base := s.PriceReader.(fakePriceReader)
	s.PriceReader = scReader{fakePriceReader: base, err: context.DeadlineExceeded}

	w := doGET(t, s, "/bill?window=custom&from=2026-09-30T00:00:00Z&to=2026-10-02T00:00:00Z")
	if w.Code != 200 {
		t.Fatalf("a standing-charge read failure must not fail the bill: %d %s", w.Code, w.Body.String())
	}
	if decode(t, w)["standing_charge_source"] != "config" {
		t.Error("want the config fallback after a read failure")
	}
}

// The archived charge changes mid-window: each rate is charged for the days it
// actually applied to, which is the whole point on 1 October.
func TestArchivedStandingChargeSplitsAtItsOwnBoundary(t *testing.T) {
	s := vatSetup(t)
	base := s.PriceReader.(fakePriceReader)
	// The supplier's charge changes at local midnight, where the agreements split.
	boundary := time.Date(2026, 9, 30, 23, 0, 0, 0, time.UTC)

	s.PriceReader = scReader{
		fakePriceReader: base,
		charges: []prices.DailyCharge{
			scCharge(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), &boundary, 56.19, 58.9995),
			scCharge(boundary, nil, 56.19, 56.19),
		},
	}

	m := decode(t, doGET(t, s, "/bill?window=custom&from=2026-09-30T00:00:00Z&to=2026-10-02T00:00:00Z"))
	if m["standing_charge_source"] != "archive" {
		t.Fatalf("source = %v, want archive", m["standing_charge_source"])
	}
	// 23 hours at 58.9995p, then 25 at 56.19p.
	got := m["standing_charge"].(float64)
	if want := (23*58.9995 + 25*56.19) / 24 / 100; got < want-0.0001 || got > want+0.0001 {
		t.Errorf("standing_charge £%.4f, want £%.4f", got, want)
	}
}
