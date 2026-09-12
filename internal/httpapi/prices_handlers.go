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
	return prices.Curve{From: from, To: to, Slots: slots}, nil
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
		"slots": sum.Slots,
		"min":   round.To(sum.Min, priceDP),
		"max":   round.To(sum.Max, priceDP),
		"mean":  round.To(sum.Mean, priceDP),
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

	knownTo, _ := s.PriceReader.KnownTo(r.Context(), code)

	writeJSONCached(w, r, http.StatusOK, map[string]any{
		"tariff_code":  code,
		"half_hourly":  true,
		"unit":         "p/kWh",
		"vat_included": true,
		"generated_at": now.In(loc),
		"from":         from.In(loc),
		"to":           to.In(loc),
		"known_to":     knownTo.In(loc),
		"summary":      summary,
		"slots":        slots,
		"cheapest":     cheapest,
		"missing":      missingOut,
		"complete":     curve.Complete(),
	})
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
	loc := s.loc()

	code, flat, found, halfHourly := s.halfHourlyTariff(win.Start)
	if !found {
		writeError(w, http.StatusServiceUnavailable,
			"no tariff is configured for that window")
		return
	}
	if !halfHourly {
		writeJSONCached(w, r, http.StatusOK, map[string]any{
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

	writeJSONCached(w, r, http.StatusOK, map[string]any{
		"window": win.Label, "from": win.Start.In(loc), "to": win.Stop.In(loc),
		"tariff_code": code, "half_hourly": true,
		"unit": "p/kWh", "vat_included": true,
		"summary": map[string]any{
			"slots": sum.Slots,
			"min":   round.To(sum.Min, priceDP),
			"max":   round.To(sum.Max, priceDP),
			"mean":  round.To(sum.Mean, priceDP),
		},
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
			"min":    round.To(d.MinExcVATPence, priceDP),
			"max":    round.To(d.MaxExcVATPence, priceDP),
			"mean":   round.To(d.MeanExcVATPence, priceDP),
			"spread": round.To(d.SpreadExcVATPence, priceDP),
			// Half hours at or below zero: free energy, or being paid to take it.
			"plunge_slots": d.PlungeSlots,
		})
	}

	writeJSONCached(w, r, http.StatusOK, map[string]any{
		"window": win.Label, "from": win.Start.In(loc), "to": win.Stop.In(loc),
		"tariff_code": code,
		// Ex-VAT here, unlike the curve endpoints: these are analytical figures
		// compared against each other over time rather than a price on a screen,
		// and the supplier publishes ex-VAT as the primary value. Stated either way
		// so nobody has to guess.
		"unit": "p/kWh", "vat_included": false,
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
func writeJSONCached(w http.ResponseWriter, r *http.Request, status int, body any) {
	encoded, err := json.Marshal(body)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not encode response")
		return
	}
	sum := sha256.Sum256(encoded)
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
