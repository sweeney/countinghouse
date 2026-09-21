package httpapi

import (
	"context"
	"net/http"
	"time"

	"github.com/sweeney/countinghouse/internal/prices"
	"github.com/sweeney/countinghouse/internal/round"
)

// ---------------------------------------------------------------------------
// Asking about the PRODUCT rather than about the agreement (issue #36 idea 3).
//
// Every price route resolves the tariff from the configured agreements first, so
// every question is really "what was I buying". A consumer wanting the curve for
// the two years BEFORE this site moved onto a half-hourly tariff — to ask how
// often cheap slots occur in winter versus summer — could not express it: under
// a flat agreement they got flat_price and no slots, before any agreement a 503,
// and across a switchover a 400. All three are right for billing. None of them
// is about the product, which exists whether or not this site was buying it.
//
// So: read by tariff code, bypassing agreement resolution entirely, plus a way to
// find out what the archive actually holds. The second matters more than it
// looks. The collector archives exactly one code, so asking by code for a window
// it never covered would otherwise return an empty slots[] — indistinguishable
// from "there were no prices then", and those lead to opposite actions.
// ---------------------------------------------------------------------------

// CoverageReader is the optional half of a PriceReader that can report what the
// archive holds.
//
// Optional and detected by type assertion, matching StandingChargeReader: an
// instance without one, or a test double that has not been taught about it, gets
// a clear refusal rather than a compile-time dependency.
type CoverageReader interface {
	Coverage(ctx context.Context) ([]prices.TariffCoverage, error)
}

// handleArchivedTariffs serves GET /prices/tariffs: what the archive holds.
func (s *Server) handleArchivedTariffs(w http.ResponseWriter, r *http.Request) {
	if !s.requirePriceArchive(w) {
		return
	}
	reader, ok := s.PriceReader.(CoverageReader)
	if !ok {
		writeError(w, http.StatusServiceUnavailable,
			"this instance's price archive cannot report coverage")
		return
	}

	held, err := reader.Coverage(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, "could not read the price archive: "+err.Error())
		return
	}

	loc := s.loc()
	collected := s.collectedCodes()
	out := make([]map[string]any, 0, len(held))
	for _, c := range held {
		out = append(out, map[string]any{
			"tariff_code":    c.TariffCode,
			"payment_method": c.PaymentMethod,
			"first_slot":     c.FirstSlot.In(loc),
			"known_to":       c.KnownTo.In(loc),
			"slots":          c.Slots,
			// Whether the collector is actively syncing this code, as against
			// holding history for one it has stopped following. A consumer polling
			// for tomorrow's publication needs to know which it is looking at.
			"collected": collected[c.TariffCode],
			// Whether the rows cover the span without a hole. Computed rather than
			// assumed: an interior gap leaves first_slot and known_to untouched, so
			// the bounds alone cannot show it.
			"complete": contiguous(c),
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"unit": "p/kWh", "vat_included": true,
		"tariffs": out,
	})
}

// contiguous reports whether the held rows fill their own span.
//
// Half-hourly slots, so the expected count is the span divided by the slot
// length. A pair holding fewer rows than that has an interior hole; more would
// mean duplicate payment methods, which the key prevents.
func contiguous(c prices.TariffCoverage) bool {
	span := c.KnownTo.Sub(c.FirstSlot)
	if span <= 0 {
		return c.Slots > 0
	}
	return c.Slots >= int(span/prices.SlotLength)
}

// collectedCodes is the set of tariff codes the collector is actively syncing.
//
// Empty when no collector is wired, which correctly reports every held code as
// not collected: an instance serving an archive it does not maintain is exactly
// the state a consumer polling for tomorrow's prices needs to know about.
func (s *Server) collectedCodes() map[string]bool {
	out := map[string]bool{}
	if s.Prices == nil {
		return out
	}
	for _, h := range s.Prices.PriceHealth() {
		out[h.TariffCode] = true
	}
	return out
}

// handleArchivedCurve serves GET /prices?tariff_code=… — the product, not the
// agreement.
//
// Deliberately different from the agreement-scoped path in three ways, all of
// which follow from asking about a product rather than about a purchase:
//
//   - the configured vat_rate is NOT applied. The archive holds the supplier's
//     own inc/exc pair; both are served, as /prices/stats already does. Grossing
//     up by this site's rate would answer "what would I have paid", which is the
//     other question.
//   - a window spanning an agreement change is fine. Agreements are irrelevant
//     here, so there is no boundary to refuse at.
//   - a window the archive does not reach is a 200 with complete:false and an
//     explicit `missing` range — never an empty slots[] that reads as "no prices
//     existed".
func (s *Server) handleArchivedCurve(w http.ResponseWriter, r *http.Request, code string) {
	win, ok := s.resolveWindow(w, r)
	if !ok {
		return
	}
	if !capWindow(w, win, maxCurveDays, "a slot per half hour", nil) {
		return
	}
	if !s.requirePriceArchive(w) {
		return
	}

	slots, err := s.PriceReader.Range(r.Context(), code, win.Start.UTC(), win.Stop.UTC())
	if err != nil {
		writeError(w, http.StatusBadGateway, "could not read the price archive: "+err.Error())
		return
	}
	knownTo, err := s.PriceReader.KnownTo(r.Context(), code)
	if err != nil {
		writeError(w, http.StatusBadGateway, "could not read the price archive: "+err.Error())
		return
	}

	loc := s.loc()
	curve := prices.NewCurve(win.Start.UTC(), win.Stop.UTC(), slots)
	priced := curve.Priced()

	out := make([]map[string]any, 0, len(priced))
	for _, sl := range priced {
		out = append(out, map[string]any{
			"valid_from": sl.ValidFrom.In(loc),
			// BOTH bases, unsuffixed inc-VAT as everywhere else on this surface —
			// these are the supplier's own figures, not ours grossed up.
			"price":         round.To(sl.IncVATPence, priceDP),
			"price_exc_vat": round.To(sl.ExcVATPence, priceDP),
		})
	}

	body := map[string]any{
		"tariff_code": code,
		"scope":       "product",
		"window":      win.Label,
		"from":        win.Start.In(loc), "to": win.Stop.In(loc),
		"unit": "p/kWh", "vat_included": true,
		"vat_source": "supplier",
		"known_to":   knownToJSON(knownTo, loc),
		"slots":      out,
		"complete":   curve.Complete(),
	}
	if miss := missingRanges(curve, loc); len(miss) > 0 {
		body["missing"] = miss
	}
	writeJSONCachedWindow(w, r, code, win, curve.Fingerprint(), body)
}

// missingRanges collapses the unheld half hours into contiguous ranges, so an
// unreachable window says so instead of arriving as an empty list — and says it
// in a handful of ranges rather than one row per missing slot.
//
// Curve.Missing returns slot STARTS; consecutive ones are merged, and each range
// ends one slot length after its last start.
func missingRanges(c prices.Curve, loc *time.Location) []map[string]any {
	gaps := c.Missing()
	if len(gaps) == 0 {
		return nil
	}
	var out []map[string]any
	start, prev := gaps[0], gaps[0]
	flush := func(last time.Time) {
		out = append(out, map[string]any{
			"from": start.In(loc),
			"to":   last.Add(prices.SlotLength).In(loc),
		})
	}
	for _, g := range gaps[1:] {
		if g.Equal(prev.Add(prices.SlotLength)) {
			prev = g
			continue
		}
		flush(prev)
		start, prev = g, g
	}
	flush(prev)
	return out
}

// handleArchivedStats serves GET /prices/stats?tariff_code=… — the product's
// aggregates, with no agreement applied.
//
// The other half of asking about a product rather than a purchase. /prices
// gained ?tariff_code= so the archive could be READ by code; without the same on
// this route the archive could not be AGGREGATED by code, and the two ideas that
// together answer "how often do cheap slots occur in winter versus summer" never
// met: /prices caps at 31 days, so two years remained ~24 paginated calls and a
// client-side rollup — the same shape of work the rollup exists to remove.
//
// It carries the same three semantics handleArchivedCurve establishes, and the
// third matters more here than there: a monthly row silently computed over a
// partial month is exactly the failure `days` was added to prevent, so an
// unreachable stretch is reported rather than quietly averaged over.
func (s *Server) handleArchivedStats(w http.ResponseWriter, r *http.Request, code string) {
	win, ok := s.resolveWindow(w, r)
	if !ok {
		return
	}
	// The stats cap, not /prices' 31: a row per day or per month is bounded by the
	// window in a way a row per slot is not, which is the whole reason a seasonal
	// window belongs on this route. Which stats cap depends on the grouping, so
	// the shape is parsed first — the same ordering the agreement-scoped route
	// uses, and for the same reason: 366 days of monthly rows is 12 rows, and
	// refusing at the daily cap would refuse the seasonal question this route
	// exists to answer.
	groupBy, cheapBelow, ok := parseStatsShape(w, r)
	if !ok {
		return
	}
	if !capStatsWindow(w, win, groupBy) {
		return
	}
	if !s.requirePriceArchive(w) {
		return
	}

	slots, err := s.PriceReader.Range(r.Context(), code, win.Start.UTC(), win.Stop.UTC())
	if err != nil {
		writeError(w, http.StatusBadGateway, "could not read the price archive: "+err.Error())
		return
	}

	loc := s.loc()
	curve := prices.NewCurve(win.Start.UTC(), win.Stop.UTC(), slots)

	body := map[string]any{
		"tariff_code": code,
		"scope":       "product",
		"window":      win.Label,
		"from":        win.Start.In(loc), "to": win.Stop.In(loc),
		"unit": "p/kWh", "vat_included": true,
		// The supplier's own inc/exc pair, not the configured vat_rate applied
		// here — grossing up by this site's rate would answer "what would I have
		// paid", which is the other question.
		"vat_source":  "supplier",
		"half_hourly": true,
		"group_by":    groupBy,
		"cheap_below": cheapBelow,
		"complete":    curve.Complete(),
	}
	if miss := missingRanges(curve, loc); len(miss) > 0 {
		body["missing"] = miss
	}

	// periods[] at BOTH groupings on this route, never days[]. The
	// agreement-scoped path's days[] carries a different per-day schema
	// (prices.DayStats), and serving two different shapes under one key is how a
	// consumer ends up parsing whichever it happened to meet first.
	body["periods"] = roundPeriods(curve.PeriodStatsOver(loc, groupBy, cheapBelow))
	writeJSONCachedWindow(w, r, code, win, curve.Fingerprint(), body)
}
