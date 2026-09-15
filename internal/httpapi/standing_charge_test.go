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

// THE case this was built for. Config still says 5%; the supplier has zero-rated
// the standing charge. The bill must use the supplier's figure.
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

	w := doGET(t, s, "/bill?window=custom&from=2026-09-30T00:00:00Z&to=2026-10-02T00:00:00Z")
	if w.Code != 200 {
		t.Fatalf("got %d: %s", w.Code, w.Body.String())
	}
	m := decode(t, w)

	if m["standing_charge_source"] != "archive" {
		t.Errorf("standing_charge_source = %v, want archive", m["standing_charge_source"])
	}
	// Two days at 56.19p/day inc VAT = £1.1238. Config would have said
	// 2 x 0.59 x 1.05 = £1.239 for the first day's rate — a different number.
	got := m["standing_charge"].(float64)
	if want := 1.1238; got < want-0.001 || got > want+0.001 {
		t.Errorf("standing_charge £%.4f, want £%.4f (the supplier's own inc-VAT figure)", got, want)
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
	boundary := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

	s.PriceReader = scReader{
		fakePriceReader: base,
		charges: []prices.DailyCharge{
			scCharge(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), &boundary, 56.19, 59.0),
			scCharge(boundary, nil, 56.19, 56.19),
		},
	}

	m := decode(t, doGET(t, s, "/bill?window=custom&from=2026-09-30T00:00:00Z&to=2026-10-02T00:00:00Z"))
	if m["standing_charge_source"] != "archive" {
		t.Fatalf("source = %v, want archive", m["standing_charge_source"])
	}
	// One day at 59p, one at 56.19p.
	got := m["standing_charge"].(float64)
	if want := 0.59 + 0.5619; got < want-0.001 || got > want+0.001 {
		t.Errorf("standing_charge £%.4f, want £%.4f", got, want)
	}
}
