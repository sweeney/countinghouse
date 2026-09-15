package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sweeney/countinghouse/internal/config"
	"github.com/sweeney/countinghouse/internal/energy"
	"github.com/sweeney/countinghouse/internal/prices"
	"github.com/sweeney/countinghouse/internal/round"
)

// PriceReader is the slice of the price archive these endpoints need.
//
// Read-only by construction: the HTTP layer must not be able to write to the
// archive even by accident, so the Put and restatement methods are deliberately
// absent. *prices.SQLiteStore satisfies it.
type PriceReader interface {
	Range(ctx context.Context, tariffCode string, from, to time.Time) ([]prices.Slot, error)
	KnownTo(ctx context.Context, tariffCode string) (time.Time, error)
}

// StandingChargeReader is the optional half of a PriceReader that can also serve
// archived standing charges.
//
// Optional and detected by type assertion, so an instance without one — or a test
// double that has not been taught about them — falls back to the configured rate,
// which is exactly what every instance did before these were archived.
type StandingChargeReader interface {
	StandingCharges(ctx context.Context, tariffCode string, from, to time.Time) ([]prices.DailyCharge, error)
}

const (
	// maxUpcomingHours bounds /prices/upcoming. The supplier publishes at most
	// about a day and a half ahead, so anything beyond two days is a
	// misunderstanding rather than a request worth serving.
	maxUpcomingHours = 48

	// defaultUpcomingHours is the common case: the rest of today and into tomorrow.
	defaultUpcomingHours = 24

	// maxCheapestDuration bounds /prices/cheapest. A deferrable load longer than
	// this is not something a half-hourly tariff can help with.
	maxCheapestDuration = 24 * time.Hour

	// priceDP is the display precision for pence. Supplier values carry up to five
	// decimals on the VAT-inclusive side; three is plenty for a screen and keeps
	// the payload readable.
	priceDP = 3
)

// runDurations are the stretches /prices/upcoming pre-computes a cheapest window
// for. Chosen to match real deferrable loads: a single slot, a dishwasher cycle,
// a washing machine, a long dryer or immersion run.
var runDurations = []struct {
	label string
	d     time.Duration
}{
	{"30m", 30 * time.Minute},
	{"1h", time.Hour},
	{"2h", 2 * time.Hour},
	{"3h", 3 * time.Hour},
}

// halfHourlyTariff resolves the tariff in force at t and reports whether it is
// priced per half hour.
//
// Returns the resolved tariff so a FLAT tariff can still be reported usefully: a
// flat-rate deployment asking for prices should learn its rate, not receive an
// empty curve that looks like a broken archive.
// spansAgreementBoundary reports whether more than one agreement covers [from, to).
//
// /prices and /prices/stats resolve the tariff at the window's START only, so a window
// spanning a switchover was described entirely by its first instant: it answered
// `half_hourly: false` with a `flat_price` and an empty slot list, while /bill over the
// identical window priced the later part per slot and reported attribution
// counter_slot. Two endpoints describing the same window as two different kinds of
// tariff, and the asymmetry grew rather than shrank once /bill learned to segment.
//
// One-tariff-per-request is the intended contract for these two routes: a curve is a
// property of a tariff, and a response mixing two would need a shape that says which
// slots belong to which. So the honest answer is to REFUSE a window that spans a
// boundary and say why, rather than silently answer about the first half.
func (s *Server) spansAgreementBoundary(from, to time.Time) bool {
	segs, err := s.Config.Tariffs().PeriodsBetween(from, to)
	if err != nil {
		// Uncovered or misconfigured: not this function's business, and the caller's
		// existing refusal path is the right one.
		return false
	}
	// More than one segment is NOT by itself a reason to refuse. What matters is
	// whether the segments describe the same curve — see curveIdentity.
	for _, seg := range segs[1:] {
		if curveIdentity(seg.Tariff) != curveIdentity(segs[0].Tariff) {
			return true
		}
	}
	return false
}

// curveIdentity is what a price response is ABOUT: the thing that must hold
// constant across a window for one response to describe it honestly.
//
// Counting segments was the obvious proxy and the wrong one, because an agreement
// can be split for reasons that leave the curve untouched. The temporary zero rate
// of VAT on domestic electricity in Great Britain — 0% for supplies from 1 October
// 2026 to 31 March 2027 — is expressed as dated blocks differing ONLY in vat_rate,
// under one unchanged tariff code. The tax changed; the tariff did not. Refusing
// those windows would have blacked out every month-spanning price request for the
// six months of the zero rate, and again for six months after it ended.
//
// It is safe for a half-hourly curve specifically because the served prices are the
// supplier's own inc-VAT figures out of the archive, not grossed up from the config
// rate — so a VAT change needs no reconciliation here; it simply arrives in the
// prices.
//
// A FLAT tariff is the exception, and stays refused: flat_price is a single inc-VAT
// number derived from the config rate, so a VAT change genuinely yields two answers
// and one field cannot carry both.
func curveIdentity(t config.Tariff) string {
	if t.IsHalfHourly() {
		return "hh:" + t.TariffCode
	}
	return fmt.Sprintf("flat:%.6f", t.UnitRate*t.Multiplier())
}

// refuseIfSpansBoundary writes a 400 when the window straddles an agreement change,
// naming the boundary so the caller can split the request.
func (s *Server) refuseIfSpansBoundary(w http.ResponseWriter, win energy.Window) bool {
	if !s.spansAgreementBoundary(win.Start, win.Stop) {
		return true
	}
	segs, err := s.Config.Tariffs().PeriodsBetween(win.Start, win.Stop)
	if err != nil || len(segs) < 2 {
		return true
	}
	writeError(w, http.StatusBadRequest, fmt.Sprintf(
		"this window spans a tariff change at %s, and a price curve belongs to one tariff: "+
			"request either side separately. /bill and /series do handle a window spanning a "+
			"switchover, because a cost can be summed across tariffs where a curve cannot.",
		segs[1].Start.In(s.loc()).Format(time.RFC3339)))
	return false
}

func (s *Server) halfHourlyTariff(t time.Time) (code string, flat float64, ok bool, halfHourly bool) {
	tariff, found := s.Config.Tariffs().TariffFor(t)
	if !found {
		return "", 0, false, false
	}
	if tariff.IsHalfHourly() {
		return tariff.TariffCode, 0, true, true
	}
	// Config stores £/kWh ex-VAT; the price surface speaks pence inc-VAT.
	return "", tariff.UnitRate * tariff.Multiplier() * 100, true, false
}

// requirePriceArchive writes the right refusal when prices cannot be served, and
// reports whether the caller may continue.
func (s *Server) requirePriceArchive(w http.ResponseWriter) bool {
	if s.PriceReader == nil {
		// 503 rather than 404: the route exists and would work on an instance with
		// an archive configured. This is a deployment state, not a bad request.
		writeError(w, http.StatusServiceUnavailable,
			"no price archive is configured on this instance (prices.db_path is unset), "+
				"so half-hourly prices cannot be served")
		return false
	}
	return true
}

// curveFor reads the archive into a Curve for [from, to).
func (s *Server) curveFor(ctx context.Context, code string, from, to time.Time) (prices.Curve, error) {
	slots, err := s.PriceReader.Range(ctx, code, from, to)
	if err != nil {
		return prices.Curve{}, err
	}
	// NewCurve rather than a literal: it precomputes the median, the sorted prices and
	// the slot index once, which is the difference between a year-long window costing
	// 3 seconds of CPU and costing microseconds.
	return prices.NewCurve(from, to, slots), nil
}

// slotJSON renders one slot with the derivations a consumer would otherwise have
// to compute — and would compute differently from the next consumer.
func slotJSON(c prices.Curve, s prices.Slot, loc *time.Location) map[string]any {
	out := map[string]any{
		"valid_from": s.ValidFrom.In(loc),
		"price":      round.To(s.IncVATPence, priceDP),
		"rank":       c.RankOf(s),
		"percentile": round.To(c.PercentileOf(s), 4),
		"band":       string(c.BandOf(s)),
	}
	if s.ValidTo != nil {
		out["valid_to"] = s.ValidTo.In(loc)
	}
	return out
}

func runJSON(r prices.Run, loc *time.Location) map[string]any {
	return map[string]any{
		"from":       r.From.In(loc),
		"to":         r.To.In(loc),
		"slots":      r.Slots,
		"mean_price": round.To(r.MeanIncVATPence, priceDP),
	}
}

// handleUpcomingPrices serves GET /prices/upcoming?hours=N: everything currently
// known about the near future, shaped so a consumer can decide rather than merely
// display.
//
// Bands, ranks and the cheapest-run windows are computed HERE rather than left to
// each caller. Two dashboards inventing their own definition of "cheap" is how a
// house ends up with two screens disagreeing about whether now is a good time.
func (s *Server) handleUpcomingPrices(w http.ResponseWriter, r *http.Request) {
	now := s.clock().Now()
	loc := s.loc()

	hours := defaultUpcomingHours
	if raw := r.URL.Query().Get("hours"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > maxUpcomingHours {
			writeError(w, http.StatusBadRequest, fmt.Sprintf(
				"'hours' must be a whole number between 1 and %d", maxUpcomingHours))
			return
		}
		hours = n
	}

	code, flat, found, halfHourly := s.halfHourlyTariff(now)
	if !found {
		writeError(w, http.StatusServiceUnavailable, "no tariff is configured for now")
		return
	}
	if !halfHourly {
		// A flat tariff is a legitimate state with a different shape of answer, not
		// an empty curve. Conflating the two would send somebody hunting a collector
		// bug that does not exist.
		writeJSONCached(w, r, http.StatusOK, map[string]any{
			"half_hourly":  false,
			"flat_price":   round.To(flat, priceDP),
			"unit":         "p/kWh",
			"vat_included": true,
			"from":         now.In(loc),
			"to":           now.Add(time.Duration(hours) * time.Hour).In(loc),
			"slots":        []any{},
			"note": "this tariff is flat-rate, so there is no half-hourly curve; " +
				"every half hour costs flat_price",
		})
		return
	}
	if !s.requirePriceArchive(w) {
		return
	}

	// Aligned down to the slot the window starts inside, so the first entry is the
	// price actually in force now rather than the next one.
	from := now.UTC().Truncate(prices.SlotLength)
	to := from.Add(time.Duration(hours) * time.Hour)

	curve, err := s.curveFor(r.Context(), code, from, to)
	if err != nil {
		writeError(w, http.StatusBadGateway, "could not read the price archive: "+err.Error())
		return
	}

	priced := curve.Priced()
	slots := make([]map[string]any, 0, len(priced))
	for _, sl := range priced {
		slots = append(slots, slotJSON(curve, sl, loc))
	}

	sum := curve.Summary()
	summary := map[string]any{
		"slots":  sum.Slots,
		"min":    round.To(sum.Min, priceDP),
		"max":    round.To(sum.Max, priceDP),
		"mean":   round.To(sum.Mean, priceDP),
		"median": round.To(sum.Median, priceDP),
	}
	// `current` is what a dashboard shows largest, so it must be the slot covering
	// now — not the window's first slot, which they happen to coincide with only
	// because the window is aligned.
	for _, sl := range priced {
		if sl.Covers(now) {
			summary["current"] = round.To(sl.IncVATPence, priceDP)
			break
		}
	}

	cheapest := map[string]any{}
	for _, rd := range runDurations {
		if run, ok := curve.CheapestRun(rd.d, time.Time{}); ok {
			cheapest[rd.label] = runJSON(run, loc)
		}
		// Omitted when it does not fit: inventing a shorter run under a label that
		// says 3h would be worse than saying nothing.
	}

	missing := curve.Missing()
	missingOut := make([]time.Time, 0, len(missing))
	for _, t := range missing {
		missingOut = append(missingOut, t.In(loc))
	}

	// The error is NOT discarded, and the zero time is NOT formatted.
	//
	// KnownTo goes to some trouble to make "we hold nothing yet" distinguishable from a
	// failure: it returns the zero time AND no error for the first, an error for the
	// second. This collapsed both into `0001-01-01T00:00:00Z` on the wire, which reads as
	// neither.
	knownTo, err := s.PriceReader.KnownTo(r.Context(), code)
	if err != nil {
		writeError(w, http.StatusBadGateway, "could not read the price archive: "+err.Error())
		return
	}

	// Tagged on what the answer MEANS — the tariff, the window rounded to the slot
	// grid, and how far the archive reaches — rather than on the rendered body, which
	// carries generated_at and so changed on every request.
	writeJSONCached(w, r, http.StatusOK, map[string]any{
		"tariff_code":  code,
		"half_hourly":  true,
		"unit":         "p/kWh",
		"vat_included": true,
		"generated_at": now.In(loc),
		"from":         from.In(loc),
		"to":           to.In(loc),
		// Omitted rather than rendered as year 1 when the archive holds nothing for this
		// tariff — absence is not a date. See knownToJSON.
		"known_to": knownToJSON(knownTo, loc),
		"summary":  summary,
		"slots":    slots,
		"cheapest": cheapest,
		"missing":  missingOut,
		"complete": curve.Complete(),
	},
		// Semantic key: the tariff, the window truncated to the slot grid (so a request
		// a minute later hits the same tag), and the archive's horizon — which is what
		// actually changes the answer.
		code,
		from.Truncate(prices.SlotLength).Format(time.RFC3339),
		to.Truncate(prices.SlotLength).Format(time.RFC3339),
		knownTo.Format(time.RFC3339),
		// The prices themselves, so a RESTATEMENT moves the tag. Without this the tag
		// keys only on metadata and a client serves a stale price indefinitely — which
		// an existing test caught when I first made this change.
		curve.Fingerprint(),
	)
}

// handleCheapestPrice serves GET /prices/cheapest?duration=3h&before=…: the
// cheapest contiguous window for a deferrable load.
//
// This is the endpoint an automation calls. It answers "when should this run",
// which is the one question a half-hourly tariff can actually act on.
func (s *Server) handleCheapestPrice(w http.ResponseWriter, r *http.Request) {
	now := s.clock().Now()
	loc := s.loc()

	raw := r.URL.Query().Get("duration")
	if raw == "" {
		writeError(w, http.StatusBadRequest,
			"'duration' is required, e.g. duration=3h or duration=90m")
		return
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 || d > maxCheapestDuration {
		writeError(w, http.StatusBadRequest, fmt.Sprintf(
			"'duration' must be a positive duration no longer than %s, e.g. 3h or 90m",
			maxCheapestDuration))
		return
	}

	var before time.Time
	if raw := r.URL.Query().Get("before"); raw != "" {
		before, err = time.Parse(time.RFC3339, raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "'before' must be an RFC3339 timestamp")
			return
		}
	}

	code, _, found, halfHourly := s.halfHourlyTariff(now)
	if !found || !halfHourly {
		writeError(w, http.StatusServiceUnavailable,
			"the tariff in force is not half-hourly, so there is no cheapest window to find")
		return
	}
	if !s.requirePriceArchive(w) {
		return
	}

	// Search everything currently published. Bounding by `before` inside the search
	// rather than here keeps the window honest when no deadline is given.
	from := now.UTC().Truncate(prices.SlotLength)
	to := from.Add(maxUpcomingHours * time.Hour)
	curve, err := s.curveFor(r.Context(), code, from, to)
	if err != nil {
		writeError(w, http.StatusBadGateway, "could not read the price archive: "+err.Error())
		return
	}

	run, ok := curve.CheapestRun(d, before)
	if !ok {
		// 404, not 400: the question was well formed and the answer is that no such
		// window exists in what we hold. A 400 would suggest the caller erred.
		writeError(w, http.StatusNotFound, fmt.Sprintf(
			"no contiguous %s window is available in the prices held%s", d,
			map[bool]string{true: " before that deadline", false: ""}[!before.IsZero()]))
		return
	}

	out := runJSON(run, loc)
	out["tariff_code"] = code
	out["unit"] = "p/kWh"
	out["vat_included"] = true
	out["searched_to"] = to.In(loc)
	writeJSONCached(w, r, http.StatusOK, out)
}

// handlePrices serves GET /prices: the curve over any window, past or future.
//
// The archive and the forward curve are the same table, so this is one query
// either way — which is the point of keeping prices rather than only fetching what
// is next.
func (s *Server) handlePrices(w http.ResponseWriter, r *http.Request) {
	win, ok := s.resolveWindow(w, r)
	if !ok {
		return
	}
	if !capWindow(w, win, maxCurveDays, "slots") {
		return
	}
	if !s.refuseIfSpansBoundary(w, win) {
		return
	}
	loc := s.loc()

	code, flat, found, halfHourly := s.halfHourlyTariff(win.Start)
	if !found {
		writeError(w, http.StatusServiceUnavailable,
			"no tariff is configured for that window")
		return
	}
	if !halfHourly {
		writeJSONCachedWindow(w, r, code, win, fmt.Sprintf("flat:%.6f", flat), map[string]any{
			"window": win.Label, "from": win.Start.In(loc), "to": win.Stop.In(loc),
			"half_hourly": false, "flat_price": round.To(flat, priceDP),
			"unit": "p/kWh", "vat_included": true, "slots": []any{},
			"note": "the tariff covering this window is flat-rate, so there is no " +
				"half-hourly curve; every half hour costs flat_price",
		})
		return
	}
	if !s.requirePriceArchive(w) {
		return
	}

	curve, err := s.curveFor(r.Context(), code, win.Start.UTC(), win.Stop.UTC())
	if err != nil {
		writeError(w, http.StatusBadGateway, "could not read the price archive: "+err.Error())
		return
	}

	priced := curve.Priced()
	slots := make([]map[string]any, 0, len(priced))
	for _, sl := range priced {
		slots = append(slots, slotJSON(curve, sl, loc))
	}
	sum := curve.Summary()

	summary := map[string]any{
		"slots":  sum.Slots,
		"min":    round.To(sum.Min, priceDP),
		"max":    round.To(sum.Max, priceDP),
		"mean":   round.To(sum.Mean, priceDP),
		"median": round.To(sum.Median, priceDP),
	}
	// `current` is what a live dashboard shows largest, and PriceCurveSummary — the
	// schema this response shares with /prices/upcoming — has always documented it.
	// It was populated on one of the two, so the spec promised a field this endpoint
	// did not send. Absent, never zero, when no held slot covers now: a historical
	// window has no "now" in it, and zero is a real price.
	now := s.clock().Now()
	for _, sl := range priced {
		if sl.Covers(now) {
			summary["current"] = round.To(sl.IncVATPence, priceDP)
			break
		}
	}

	// known_to is how a polling consumer SEES the daily publication: when the horizon
	// moves, there is more curve to draw. Carried here and not only on
	// /prices/upcoming because that route is forward-only, so a dashboard drawing any
	// retrospective context had to call both just to learn one number.
	knownTo, err := s.PriceReader.KnownTo(r.Context(), code)
	if err != nil {
		writeError(w, http.StatusBadGateway, "could not read the price archive: "+err.Error())
		return
	}

	// The horizon joins the cache tag deliberately. A publication can extend it
	// without touching the slots of a PAST window, and a tag keyed only on this
	// window's content would then answer 304 while the field being polled for had
	// moved — the same failure the content fingerprint was introduced to fix.
	writeJSONCachedWindow(w, r, code, win,
		curve.Fingerprint()+"|"+knownTo.UTC().Format(time.RFC3339),
		map[string]any{
			"window": win.Label, "from": win.Start.In(loc), "to": win.Stop.In(loc),
			"tariff_code": code, "half_hourly": true,
			"unit": "p/kWh", "vat_included": true,
			"known_to": knownToJSON(knownTo, loc),
			"summary":  summary,
			"slots":    slots,
			"complete": curve.Complete(),
		})
}

// handlePriceStats serves GET /prices/stats: per-local-day aggregates.
//
// Grouped by LOCAL calendar day because that is how a person thinks about a day,
// and the only framing in which a 23- or 25-hour day makes sense. `spread` is the
// number that says whether shifting load was worth the bother.
func (s *Server) handlePriceStats(w http.ResponseWriter, r *http.Request) {
	win, ok := s.resolveWindow(w, r)
	if !ok {
		return
	}
	if !capWindow(w, win, maxStatsDays, "daily rows") {
		return
	}
	if !s.refuseIfSpansBoundary(w, win) {
		return
	}
	loc := s.loc()

	code, _, found, halfHourly := s.halfHourlyTariff(win.Start)
	if !found || !halfHourly {
		writeError(w, http.StatusServiceUnavailable,
			"the tariff covering that window is not half-hourly, so it has no daily spread")
		return
	}
	if !s.requirePriceArchive(w) {
		return
	}

	curve, err := s.curveFor(r.Context(), code, win.Start.UTC(), win.Stop.UTC())
	if err != nil {
		writeError(w, http.StatusBadGateway, "could not read the price archive: "+err.Error())
		return
	}

	stats := curve.DailyStats(loc)
	days := make([]map[string]any, 0, len(stats))
	for _, d := range stats {
		days = append(days, map[string]any{
			"day": d.Day, "slots": d.Slots,
			// The unsuffixed keys are INC VAT, as on every sibling price route. They
			// were ex-VAT, and the difference was documented in three places rather
			// than removed — three places describing a surprise instead of one
			// preventing it. Nothing consumes these routes yet, so the moment to make
			// the common name mean the common thing is now: a dashboard plotting a
			// daily mean against a live price was otherwise out by the VAT rate with
			// nothing on the wire to say so.
			//
			// The ex-VAT figures keep an explicit suffix, because they are still the
			// right basis for comparing days against each other and the supplier
			// publishes ex-VAT as the primary value.
			"min":            round.To(d.MinIncVATPence, priceDP),
			"max":            round.To(d.MaxIncVATPence, priceDP),
			"mean":           round.To(d.MeanIncVATPence, priceDP),
			"spread":         round.To(d.SpreadIncVATPence, priceDP),
			"min_exc_vat":    round.To(d.MinExcVATPence, priceDP),
			"max_exc_vat":    round.To(d.MaxExcVATPence, priceDP),
			"mean_exc_vat":   round.To(d.MeanExcVATPence, priceDP),
			"spread_exc_vat": round.To(d.SpreadExcVATPence, priceDP),
			// Half hours at or below zero: free energy, or being paid to take it.
			"plunge_slots": d.PlungeSlots,
		})
	}

	writeJSONCachedWindow(w, r, code, win, curve.Fingerprint(), map[string]any{
		"window": win.Label, "from": win.Start.In(loc), "to": win.Stop.In(loc),
		"tariff_code": code,
		// Inc-VAT, matching every sibling price route. Stated either way so nobody
		// has to guess, and the ex-VAT figures are carried per day under explicit
		// _exc_vat keys.
		"unit": "p/kWh", "vat_included": true,
		"days": days,
	})
}

// resolveWindow parses the shared window parameters, writing a 400 on failure.
// Shared with the energy endpoints so /prices accepts exactly the same window
// vocabulary as /series and /bill.
func (s *Server) resolveWindow(w http.ResponseWriter, r *http.Request) (energy.Window, bool) {
	q := r.URL.Query()
	spec := q.Get("window")
	if spec == "" {
		spec = energy.WindowToday
	}
	var from, to time.Time
	var err error
	if raw := q.Get("from"); raw != "" {
		if from, err = time.Parse(time.RFC3339, raw); err != nil {
			writeError(w, http.StatusBadRequest, "'from' must be an RFC3339 timestamp")
			return energy.Window{}, false
		}
	}
	if raw := q.Get("to"); raw != "" {
		if to, err = time.Parse(time.RFC3339, raw); err != nil {
			writeError(w, http.StatusBadRequest, "'to' must be an RFC3339 timestamp")
			return energy.Window{}, false
		}
	}
	if spec != energy.WindowCustom && (!from.IsZero() || !to.IsZero()) {
		writeError(w, http.StatusBadRequest, "'from'/'to' are only valid with window=custom")
		return energy.Window{}, false
	}

	win, err := energy.ResolveWindow(s.clock().Now(), s.loc(), spec, from, to)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return energy.Window{}, false
	}

	// Prices differ from consumption in the one way that matters here: today's are
	// published in full before today begins. Period-TO-DATE is right for energy —
	// nobody has consumed this evening's electricity yet — and wrong for a curve,
	// where it hands a dashboard half a chart at lunchtime and reports it complete.
	// So `today` means the whole LOCAL day on these routes.
	//
	// Week and month stay to-date deliberately: the supplier publishes about a day
	// and a half ahead, so extending those would return a window that is mostly
	// holes and never `complete`. The asymmetry tracks a real one in the data.
	if spec == energy.WindowToday {
		_, endOfDay := prices.LocalDayWindow(s.clock().Now(), s.loc())
		win.Stop = endOfDay
	}
	return win, true
}

// writeJSONCached writes a JSON body with a strong ETag, answering 304 when the
// caller already has it.
//
// Worth having on the price routes specifically: the curve changes about once a
// day, while a dashboard polls every few seconds. Without this, each poll ships a
// 48-slot payload to say nothing has changed.
//
// The ETag is a hash of the rendered body rather than of `known_to` or a
// timestamp, so it cannot claim "unchanged" when anything in the response has in
// fact moved — including a band that shifted because the window slid forward.
// Window caps for the price routes.
//
// `/series` has guarded itself with MaxBuckets from the start; these routes accepted any
// `window=custom` span, so the bound was emergent rather than stated — and the archive
// only gets longer. Rendering a year is fast now (0.9ms, down from 2.98s) but it is still
// ~2 MB of response, and a bound nobody chose is not a bound.
//
// The two differ because their responses do: `/prices` emits a row per HALF HOUR, so a
// month is already 1,488 of them; `/prices/stats` emits a row per DAY, where a year is
// 365 and perfectly reasonable.
const (
	maxCurveDays = 31
	maxStatsDays = 366
)

// capWindow refuses a window longer than maxDays, naming the cap and what it protects.
func capWindow(w http.ResponseWriter, win energy.Window, maxDays int, unit string) bool {
	days := win.Stop.Sub(win.Start).Hours() / 24
	if days <= float64(maxDays) {
		return true
	}
	writeError(w, http.StatusBadRequest, fmt.Sprintf(
		"window spans %.0f days, over the cap of %d for this endpoint (it returns one of its "+
			"%s per half hour or per day, and an unbounded window is an unbounded response); "+
			"request a shorter range", days, maxDays, unit))
	return false
}

// knownToJSON renders the archive's horizon, or nil when there is none.
//
// The zero time formats as `0001-01-01T00:00:00Z`, which a consumer cannot read as "we
// hold nothing yet" — it looks like corruption or a parsing bug. null says it plainly,
// and matches how Reconciliation already omits its meter-derived fields when there is no
// meter: absence is not a value.
func knownToJSON(t time.Time, loc *time.Location) any {
	if t.IsZero() {
		return nil
	}
	return t.In(loc)
}

// writeJSONCachedWindow is writeJSONCached keyed on a window rather than on the body.
//
// `to` is "now" for the default window=today, so hashing the rendered body meant the tag
// moved every request and the 304 never fired. Truncating to the slot grid makes two
// requests within the same half hour agree, which is the granularity at which the answer
// can actually change.
func writeJSONCachedWindow(w http.ResponseWriter, r *http.Request, code string, win energy.Window, fingerprint string, body any) {
	writeJSONCached(w, r, http.StatusOK, body,
		code,
		win.Label,
		win.Start.Truncate(prices.SlotLength).Format(time.RFC3339),
		win.Stop.Truncate(prices.SlotLength).Format(time.RFC3339),
		fingerprint,
	)
}

func writeJSONCached(w http.ResponseWriter, r *http.Request, status int, body any, tag ...string) {
	encoded, err := json.Marshal(body)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not encode response")
		return
	}
	// The tag is computed from the SEMANTIC payload when the caller supplies one, not
	// from the rendered body.
	//
	// Hashing the body looked obviously right and was the bug: three of the four price
	// responses carry a timestamp that moves on every request — `generated_at`, and
	// `to` when the window ends at "now" — so the tag changed every time and the 304
	// could never fire. On the endpoint the caching was built for, a dashboard
	// re-downloaded all 48 slots on every poll.
	//
	// `generated_at` is genuinely useful to a human reading the response, so dropping it
	// to make caching work would be fixing the wrong end.
	material := encoded
	if len(tag) > 0 {
		material = []byte(strings.Join(tag, "\x00"))
	}
	sum := sha256.Sum256(material)
	etag := `"` + hex.EncodeToString(sum[:16]) + `"`

	w.Header().Set("ETag", etag)
	// Short, not long: a publication can land at any moment in the late afternoon,
	// and a stale curve is worse than a re-fetch. The ETag does the real work.
	w.Header().Set("Cache-Control", "private, max-age=30")

	if match := r.Header.Get("If-None-Match"); match != "" && strings.Contains(match, etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(encoded) //nolint:errcheck
}
