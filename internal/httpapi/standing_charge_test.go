package httpapi

import (
	"context"
	"testing"
	"time"

	"github.com/sweeney/countinghouse/internal/config"
	"github.com/sweeney/countinghouse/internal/prices"
)

// ---------------------------------------------------------------------------
// The standing charge was the LAST number in a bill still grossed up from
// configuration, and therefore the last one a stale vat_rate could silently get
// wrong. Archiving the supplier's own figure demotes config's vat_rate to a
// checkable expectation everywhere.
// ---------------------------------------------------------------------------

// scReader is a PriceReader that also serves standing charges. A charge in
// charges was seen when it was retrieved, which scCharge makes its start: the
// archive has watched it all along. histories carries the ones whose past matters.
type scReader struct {
	fakePriceReader
	charges   []prices.DailyCharge
	histories []prices.ChargeHistory
	err       error
}

func (r scReader) StandingChargeHistories(_ context.Context, code string, from, to time.Time) ([]prices.ChargeHistory, error) {
	if r.err != nil {
		return nil, r.err
	}
	all := append([]prices.ChargeHistory(nil), r.histories...)
	for _, c := range r.charges {
		all = append(all, prices.ChargeHistory{Charge: c, FirstSeenAt: c.RetrievedAt})
	}
	var out []prices.ChargeHistory
	for _, h := range all {
		c := h.Charge
		if c.TariffCode == code && c.ValidFrom.Before(to) && (c.ValidTo == nil || c.ValidTo.After(from)) {
			out = append(out, h)
		}
	}
	return out, nil
}

func scTime(t *testing.T, s string) time.Time {
	t.Helper()
	at, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return at
}

// scProductionHistory is the Agile standing charge as production's archive holds
// it: one open-ended charge from 2024, first seen at 5% on 17 Sep 2026, restated
// in place to its zero-rated figure by the daily sweep that ran two hours after
// the cut. The sweep before it had last confirmed the 5% figure a day earlier.
func scProductionHistory(t *testing.T) prices.ChargeHistory {
	t.Helper()
	c := scCharge(time.Date(2024, 9, 30, 23, 0, 0, 0, time.UTC), nil, 59.1606, 59.1606)
	c.RetrievedAt = scTime(t, "2026-10-26T01:03:00Z")
	return prices.ChargeHistory{
		Charge:      c,
		FirstSeenAt: scTime(t, "2026-09-17T01:03:00Z"),
		Restatements: []prices.Restatement{{
			Key:            prices.Key{TariffCode: pxTariff, ValidFrom: c.ValidFrom},
			OldExcVATPence: 59.1606, OldIncVATPence: 62.1186,
			NewExcVATPence: 59.1606, NewIncVATPence: 59.1606,
			PreviousRetrievedAt: scTime(t, "2026-09-30T01:03:00Z"),
			DetectedAt:          scTime(t, "2026-10-01T01:03:00Z"),
		}},
	}
}

// openAgreementAt is configuration that never authored the 1 Oct split: one
// open-ended Agile agreement at vatRate.
func openAgreementAt(vatRate float64) config.EnergyAgreements {
	began := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return config.EnergyAgreements{Agreements: map[string][]config.Agreement{
		"electricity": {
			{From: &began, Name: "Agile", Type: config.TariffTypeVariable,
				ID: pxTariff, Unit: "kWh", VATRate: vatRate, DailyStandingCharge: 0.59},
		},
	}}
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

// #51, as it happened in production. The archived row now says 0% for every day
// back to 2024; the 5% figure the supplier was publishing before the cut lives
// only in the restatement log, and a day before the cut must be billed at it.
func TestBackdatedZeroRatedStandingChargeKeepsVATBeforeTheCut(t *testing.T) {
	s := vatSetup(t)
	base := s.PriceReader.(fakePriceReader)
	s.PriceReader = scReader{
		fakePriceReader: base,
		histories:       []prices.ChargeHistory{scProductionHistory(t)},
	}

	// 21 Sep 2026, a whole local day under the 5% agreement.
	w := doGET(t, s, "/bill?window=custom&from=2026-09-20T23:00:00Z&to=2026-09-21T23:00:00Z")
	if w.Code != 200 {
		t.Fatalf("got %d: %s", w.Code, w.Body.String())
	}
	m := decode(t, w)
	got := m["standing_charge"].(float64)
	if want := 0.621186; got < want-0.0001 || got > want+0.0001 {
		t.Errorf("standing_charge £%.4f, want £%.4f: the 62.1186p the supplier was "+
			"publishing for that day, not the zero-rated figure backdated to 2024", got, want)
	}
	if m["standing_charge_source"] != "archive" {
		t.Errorf("standing_charge_source = %v, want archive: the supplier's own figure", m["standing_charge_source"])
	}
}

// Across the cut, each side gets the figure that was current for it. The sweep
// that saw the change ran a day after the one before it, so the archive cannot
// say when in that day it happened; the agreements can, and put it at midnight.
func TestBackdatedStandingChargeAcrossTheCutBillsEachSideAtItsOwnFigure(t *testing.T) {
	s := vatSetup(t)
	base := s.PriceReader.(fakePriceReader)
	s.PriceReader = scReader{
		fakePriceReader: base,
		histories:       []prices.ChargeHistory{scProductionHistory(t)},
	}

	// 00:00Z 30 Sep to 00:00Z 2 Oct. The agreements split at local midnight,
	// 23:00Z: 23 hours at 5%, then 25 at 0%.
	m := decode(t, doGET(t, s, "/bill?window=custom&from=2026-09-30T00:00:00Z&to=2026-10-02T00:00:00Z"))
	got := m["standing_charge"].(float64)
	want := (23*62.1186 + 25*59.1606) / 24 / 100
	if got < want-0.0001 || got > want+0.0001 {
		t.Errorf("standing_charge £%.4f, want £%.4f (23h at 62.1186p, 25h at 59.1606p)", got, want)
	}
	if m["standing_charge_source"] != "archive" {
		t.Errorf("standing_charge_source = %v, want archive", m["standing_charge_source"])
	}
}

// The same backdated row in an archive rebuilt from Octopus after the cut: no
// restatement log, first seen at 0%. For days before that nobody saw what the
// supplier charged, so the agreement decides whether the carried-back figure is
// believable. It is not, and the day is grossed up from ex-VAT, and says so.
func TestBackdatedStandingChargeWithoutItsLogIsGrossedUpFromExVAT(t *testing.T) {
	s := vatSetup(t)
	base := s.PriceReader.(fakePriceReader)
	c := scCharge(time.Date(2024, 9, 30, 23, 0, 0, 0, time.UTC), nil, 59.1606, 59.1606)
	s.PriceReader = scReader{
		fakePriceReader: base,
		histories: []prices.ChargeHistory{{
			Charge: c, FirstSeenAt: scTime(t, "2026-10-20T01:03:00Z"),
		}},
	}

	m := decode(t, doGET(t, s, "/bill?window=custom&from=2026-09-20T23:00:00Z&to=2026-09-21T23:00:00Z"))
	got := m["standing_charge"].(float64)
	if want := 0.591606 * 1.05; got < want-0.0001 || got > want+0.0001 {
		t.Errorf("standing_charge £%.4f, want £%.4f: 59.1606p ex VAT at the agreement's 5%%", got, want)
	}
	if m["standing_charge_source"] != "archive_ex_vat" {
		t.Errorf("standing_charge_source = %v, want archive_ex_vat: the bill used the "+
			"supplier's ex-VAT figure, not its inc-VAT one, and must say so", m["standing_charge_source"])
	}
}

// Configuration that forgot the split, the other way round from #51: the
// supplier zero-rated, the agreement still says 5%. Days the archive saw the
// supplier's figure for are billed at it, whatever configuration says. Config is
// what the drift alert is for; it is not a billing input for witnessed days, so
// forgetting a split cannot over- or under-bill the standing charge.
func TestWitnessedStandingChargeIsBilledOverAStaleAgreement(t *testing.T) {
	s := vatSetup(t)
	s.Config = pxConfig{agreements: openAgreementAt(0.05)}
	base := s.PriceReader.(fakePriceReader)
	s.PriceReader = scReader{
		fakePriceReader: base,
		histories:       []prices.ChargeHistory{scProductionHistory(t)},
	}

	// 1 Oct, local: after the restatement was seen, under a 5% agreement.
	m := decode(t, doGET(t, s, "/bill?window=custom&from=2026-10-01T01:03:00Z&to=2026-10-01T23:00:00Z"))
	got := m["standing_charge"].(float64)
	hours := 23.0 - (1 + 3.0/60)
	if want := hours / 24 * 0.591606; got < want-0.0001 || got > want+0.0001 {
		t.Errorf("standing_charge £%.4f, want £%.4f: the supplier's zero-rated figure, "+
			"not 5%% from a configuration nobody updated", got, want)
	}
	if m["standing_charge_source"] != "archive" {
		t.Errorf("standing_charge_source = %v, want archive", m["standing_charge_source"])
	}
}

// A carried-back inc-VAT figure that rounds the agreement's rate is the
// agreement's rate: it is billed as-is, not recomputed. Same tolerance as the
// drift alert.
func TestStandingChargeWithinRoundingOfTheAgreementIsBilledAsIs(t *testing.T) {
	s := vatSetup(t)
	base := s.PriceReader.(fakePriceReader)
	// 59.1606 x 1.05 = 62.11863; the supplier rounds to 62.1186. First seen
	// after the day being billed, so the agreement is what judges it.
	c := scCharge(time.Date(2024, 9, 30, 23, 0, 0, 0, time.UTC), nil, 59.1606, 62.1186)
	s.PriceReader = scReader{
		fakePriceReader: base,
		histories: []prices.ChargeHistory{{
			Charge: c, FirstSeenAt: scTime(t, "2026-09-25T01:03:00Z"),
		}},
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
