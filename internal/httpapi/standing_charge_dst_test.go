package httpapi

import (
	"math"
	"testing"
	"time"

	"github.com/sweeney/countinghouse/internal/prices"
)

// ---------------------------------------------------------------------------
// Issue #56, end to end: /bill for the autumn clock-change day.
//
// Seen in prod: /bill?window=custom over Sun 25 Oct 2026 (local) reported a
// standing_charge of £0.6163 — the supplier's £0.5916 × 25/24 — because both
// standing-charge paths divided elapsed hours by 24. Octopus bills that day at
// £0.5916: a standing charge is a price per calendar day of supply.
//
// Both sources are pinned here because the bill silently switches between them
// (the archive when it covers the window, config otherwise), and a fix to one
// alone would make the same day's charge depend on which was available.
// ---------------------------------------------------------------------------

// The local day 25 Oct 2026, as the query string a client would send: BST
// midnight is 23:00Z the previous day, and GMT midnight is 00:00Z.
const autumnClockChangeDay = "/bill?window=custom&from=2026-10-24T23:00:00Z&to=2026-10-26T00:00:00Z"

// dstBillSetup is vatSetup moved to just after the autumn clock change, so the
// window is in the past. The config says 59p/day at 0% VAT for this date.
func dstBillSetup(t *testing.T) *Server {
	t.Helper()
	s := vatSetup(t)
	s.Clock = fixedClock{time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)}
	return s
}

func standingChargeOf(t *testing.T, s *Server) (float64, string) {
	t.Helper()
	w := doGET(t, s, autumnClockChangeDay)
	if w.Code != 200 {
		t.Fatalf("got %d: %s", w.Code, w.Body.String())
	}
	m := decode(t, w)
	src, _ := m["standing_charge_source"].(string)
	return m["standing_charge"].(float64), src
}

func TestBillChargesTheAutumnClockChangeDayAsOneDay_Archive(t *testing.T) {
	s := dstBillSetup(t)
	base := s.PriceReader.(fakePriceReader)
	// The prod figure: 59.16p/day inc VAT.
	s.PriceReader = scReader{
		fakePriceReader: base,
		charges: []prices.DailyCharge{
			scCharge(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), nil, 59.16, 59.16),
		},
	}

	got, src := standingChargeOf(t, s)
	if src != "archive" {
		t.Fatalf("standing_charge_source = %q, want archive", src)
	}
	if want := 0.5916; math.Abs(got-want) > 1e-9 {
		t.Errorf("standing_charge £%.4f, want £%.4f — one day, as Octopus bills it (25/24 would be £0.6163)",
			got, want)
	}
}

func TestBillChargesTheAutumnClockChangeDayAsOneDay_Config(t *testing.T) {
	s := dstBillSetup(t) // no StandingChargeReader: the bill falls back to config

	got, src := standingChargeOf(t, s)
	if src != "config" {
		t.Fatalf("standing_charge_source = %q, want config", src)
	}
	if want := 0.59; math.Abs(got-want) > 1e-9 {
		t.Errorf("standing_charge £%.4f, want £%.4f — one day at the configured, zero-rated 59p", got, want)
	}
}
