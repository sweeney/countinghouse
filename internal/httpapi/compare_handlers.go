package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/sweeney/countinghouse/internal/config"
	"github.com/sweeney/countinghouse/internal/energy"
	"github.com/sweeney/countinghouse/internal/round"
)

// ---------------------------------------------------------------------------
// GET /compare — "what would this window have cost under X?"
//
// The question nearly every consumer of this service was really asking, and the
// one they had to hand-roll every time: the same kWh on a named tariff, on a rate
// from a renewal letter, or spread flat across the window's own prices. Five
// hand-rolls produced two baseline errors — an expired comparator, and a dropped
// standing-charge difference — and both are errors a server-side primitive makes
// structurally hard rather than merely documented against.
//
// The service already holds the dated agreements, the curve and the consumption.
// The client's only genuine contribution is the alternative.
// ---------------------------------------------------------------------------

// handleCompare serves GET /compare.
func (s *Server) handleCompare(w http.ResponseWriter, r *http.Request) {
	win, ok := s.resolveWindowParams(w, r)
	if !ok {
		return
	}

	// REQUIRED, with no default. On one real window the same question answered
	// -0.4% at household scope and -31.8% at monitored scope: both correct, and
	// they support opposite conclusions. A default would pick one silently, once,
	// for every caller, forever — so the friction is deliberate.
	scope := r.URL.Query().Get("scope")
	if scope == "" {
		writeError(w, http.StatusBadRequest,
			"'scope' is required and has no default: 'household' compares the whole "+
				"home (the meter), 'monitored' compares only the devices this service "+
				"meters individually. The two can differ by enough to support opposite "+
				"conclusions on the same window, so there is no safe default to pick "+
				"for you")
		return
	}
	if !energy.ValidScope(scope) {
		writeError(w, http.StatusBadRequest, "invalid 'scope' (want household or monitored)")
		return
	}

	specs := r.URL.Query()["alt"]
	if len(specs) == 0 {
		writeError(w, http.StatusBadRequest,
			"at least one 'alt' is required, e.g. alt=window_mean, "+
				"alt=tariff:id=E-1R-VAR-22-11-01-C, or "+
				"alt=flat:unit_rate=0.2150,daily_standing_charge=0.4200,vat_rate=0.05")
		return
	}

	plan, err := s.planFor(r.Context(), win.Start, win.Stop)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}

	actual, ok := s.actualFor(w, r, win, plan, scope)
	if !ok {
		return
	}

	days := energy.WindowDays(win)
	alts := make([]energy.Alternative, 0, len(specs))
	for _, spec := range specs {
		alt, ok := s.alternativeFor(w, spec, win, plan, actual, days)
		if !ok {
			return
		}
		alts = append(alts, alt)
	}

	// Same cacheability as the bill it is a difference from, for the same reason:
	// derived from Influx over a window, so max-age and no ETag (issue #36 N7). A
	// counterfactual that cached differently from /bill would let the two drift in
	// a consumer's cache even though one is defined in terms of the other.
	s.writeJSONWindowCacheable(w, win, map[string]any{
		"window":   win.Label,
		"from":     win.Start.In(s.loc()),
		"to":       win.Stop.In(s.loc()),
		"days":     round.To(days, 3),
		"scope":    scope,
		"currency": "GBP",
		"actual":   roundCosted(actual),
		// Named for what it is: the energy the comparison is over. A consumer
		// reading a delta needs to know it applies to this much electricity.
		"alternatives": roundAlternatives(alts),
	})
}

// actualFor assembles what the window really cost at the requested scope.
//
// It runs the /bill path rather than a parallel one, so a comparison and a bill
// cannot disagree about the baseline — which would be the worst possible failure
// for an endpoint whose entire output is a difference from that baseline.
func (s *Server) actualFor(w http.ResponseWriter, r *http.Request, win energy.Window, plan tariffPlan, scope string) (energy.Costed, bool) {
	bill, ok := s.assembleBill(w, r, win, plan)
	if !ok {
		return energy.Costed{}, false
	}

	actual := energy.Costed{
		Label:          "actual",
		TariffCodes:    s.tariffCodesFor(win.Start, win.Stop),
		StandingCharge: bill.StandingCharge,
	}

	if scope == energy.ScopeMonitored {
		actual.KWh = bill.Reconciliation.MonitoredKWh
		actual.EnergyCost = bill.EnergyCost
		actual.Total = actual.EnergyCost + actual.StandingCharge
		return actual, true
	}

	// Household scope needs the meter. Without one the rest-of-home is undefined,
	// and answering from the monitored devices alone would silently relabel a
	// partial figure as the whole house — the exact confusion /bill's `scope`
	// field exists to prevent.
	if !bill.Reconciliation.MeterPresent || bill.HouseholdEnergyCost == nil {
		writeError(w, http.StatusBadRequest,
			"scope=household needs a whole-house meter, and none is configured (or its "+
				"remainder could not be priced); request scope=monitored, which compares "+
				"the devices this service meters individually")
		return energy.Costed{}, false
	}
	if bill.Reconciliation.MeterKWh != nil {
		actual.KWh = *bill.Reconciliation.MeterKWh
	}
	actual.EnergyCost = *bill.HouseholdEnergyCost
	actual.Total = actual.EnergyCost + actual.StandingCharge
	return actual, true
}

// alternativeFor parses one alt spec and prices it against the actual.
func (s *Server) alternativeFor(w http.ResponseWriter, spec string, win energy.Window, plan tariffPlan, actual energy.Costed, days float64) (energy.Alternative, bool) {
	kind, params, err := parseAltSpec(spec)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return energy.Alternative{}, false
	}

	switch kind {
	case energy.KindFlat:
		return s.flatAlternative(w, params, actual, days)
	case energy.KindTariff:
		return s.tariffAlternative(w, params, actual, days)
	case energy.KindWindowMean:
		return s.windowMeanAlternative(w, win, plan, actual)
	default:
		writeError(w, http.StatusBadRequest, fmt.Sprintf(
			"unknown alt kind %q (want flat, tariff or window_mean)", kind))
		return energy.Alternative{}, false
	}
}

// flatAlternative prices the caller's own rates — the renewal-letter case.
func (s *Server) flatAlternative(w http.ResponseWriter, params map[string]string, actual energy.Costed, days float64) (energy.Alternative, bool) {
	unitRate, ok := requireFloat(w, params, "unit_rate", "flat")
	if !ok {
		return energy.Alternative{}, false
	}
	standing, ok := requireFloat(w, params, "daily_standing_charge", "flat")
	if !ok {
		return energy.Alternative{}, false
	}
	// REQUIRED rather than defaulted to 5%. A VAT rate assumed is a bill wrong by
	// a few percent with every figure well-formed and internally consistent —
	// which is precisely the failure mode the price validator refuses, and a
	// statutory zero rate currently in force makes 5% a bad guess besides.
	vat, ok := requireFloat(w, params, "vat_rate", "flat")
	if !ok {
		return energy.Alternative{}, false
	}

	label := params["label"]
	if label == "" {
		label = "flat rate"
	}
	alt := energy.FlatCost(label, actual.KWh, days, unitRate, standing, vat)
	// Availability does not apply: a rate off a letter has no dates to check.
	return energy.CompareTo(actual, alt, energy.KindFlat, nil, ""), true
}

// tariffAlternative prices a dated agreement this service already knows about.
func (s *Server) tariffAlternative(w http.ResponseWriter, params map[string]string, actual energy.Costed, days float64) (energy.Alternative, bool) {
	id := params["id"]
	if id == "" {
		writeError(w, http.StatusBadRequest,
			"alt=tariff needs an 'id', e.g. alt=tariff:id=E-1R-VAR-22-11-01-C; "+
				"GET /tariffs lists the agreements this service holds, each with "+
				"available_now")
		return energy.Alternative{}, false
	}

	ag, found := s.findAgreement(id)
	if !found {
		writeError(w, http.StatusNotFound, fmt.Sprintf(
			"no configured agreement has id or name %q; GET /tariffs lists them", id))
		return energy.Alternative{}, false
	}

	// A half-hourly alternative needs that tariff's own archived curve, and the
	// collector archives exactly one code — this site's. Refusing is the honest
	// answer: pricing it from the configured unit rate would charge nothing,
	// because a variable agreement deliberately carries no rate.
	if ag.Type == config.TariffTypeVariable {
		writeError(w, http.StatusNotImplemented, fmt.Sprintf(
			"agreement %q is half-hourly, so comparing against it needs its own price "+
				"curve, and this service archives only the tariff it is on. Compare "+
				"against a fixed agreement, or supply the rate directly with alt=flat",
			id))
		return energy.Alternative{}, false
	}

	label := params["label"]
	if label == "" {
		label = ag.Name
	}
	alt := energy.FlatCost(label, actual.KWh, days, ag.UnitRate, ag.DailyStandingCharge, ag.VATRate)
	alt.TariffCodes = codesOf(ag)

	available, note := s.availability(ag)
	return energy.CompareTo(actual, alt, energy.KindTariff, &available, note), true
}

// windowMeanAlternative prices the same kWh at the window's own time-weighted
// mean rate: "we used this much, but evenly".
//
// The standing charge is unchanged — this is the same tariff, differently shaped
// — so delta.standing is zero and the whole difference is the load shape. That is
// the number that says whether shifting load actually paid.
func (s *Server) windowMeanAlternative(w http.ResponseWriter, win energy.Window, plan tariffPlan, actual energy.Costed) (energy.Alternative, bool) {
	rate, complete := energy.MeanRateOverWindow(win, plan.pricer)
	if !complete {
		writeError(w, http.StatusServiceUnavailable,
			"some half hours in this window hold no price, so the window has no honest "+
				"mean rate to compare against; a mean over the half that is priced would "+
				"be a plausible-looking wrong number. Request a window the archive covers "+
				"(see /healthz prices.complete_to)")
		return energy.Alternative{}, false
	}

	energyCost := actual.KWh * rate
	alt := energy.Costed{
		Label:          "same kWh, spread flat across this window's own prices",
		TariffCodes:    actual.TariffCodes,
		KWh:            actual.KWh,
		EnergyCost:     energyCost,
		StandingCharge: actual.StandingCharge,
		Total:          energyCost + actual.StandingCharge,
	}
	return energy.CompareTo(actual, alt, energy.KindWindowMean, nil, ""), true
}

// findAgreement looks an agreement up by supplier code or by name.
//
// By either, because a consumer reading /tariffs sees both and a variable
// agreement's code is the thing that identifies it while a fixed one's may be
// absent entirely.
func (s *Server) findAgreement(id string) (config.Agreement, bool) {
	for _, list := range s.Config.Agreements().Agreements {
		for _, ag := range list {
			if ag.ID == id || ag.Name == id {
				return ag, true
			}
		}
	}
	return config.Agreement{}, false
}

// availability reports whether an agreement is on sale now, and says why not.
//
// The note exists because "false" alone sends a caller back to the dates to work
// out which end they fell off.
func (s *Server) availability(ag config.Agreement) (bool, string) {
	now := s.clock().Now()
	if ag.From != nil && now.Before(*ag.From) {
		return false, "this agreement does not start until " + ag.From.Format("2006-01-02")
	}
	if ag.To != nil && !now.Before(*ag.To) {
		return false, "this agreement ended " + ag.To.Format("2006-01-02") + " and is not on sale"
	}
	return true, ""
}

func codesOf(ag config.Agreement) []string {
	if ag.ID == "" {
		return nil
	}
	return []string{ag.ID}
}

// parseAltSpec splits "kind:k=v,k=v" into its kind and parameters.
//
// A bare kind ("window_mean") is legal and carries no parameters.
func parseAltSpec(spec string) (kind string, params map[string]string, err error) {
	params = map[string]string{}
	kind, rest, hasParams := strings.Cut(spec, ":")
	kind = strings.TrimSpace(kind)
	if kind == "" {
		return "", nil, errors.New("empty 'alt'; want e.g. alt=window_mean, " +
			"alt=tariff:id=E-1R-VAR-22-11-01-C or alt=flat:unit_rate=…")
	}
	if !hasParams {
		return kind, params, nil
	}
	for _, pair := range strings.Split(rest, ",") {
		if strings.TrimSpace(pair) == "" {
			continue
		}
		k, v, ok := strings.Cut(pair, "=")
		if !ok {
			return "", nil, fmt.Errorf("malformed 'alt' parameter %q in %q; want key=value", pair, spec)
		}
		params[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return kind, params, nil
}

// requireFloat reads a required numeric alt parameter, refusing rather than
// assuming. See flatAlternative on why vat_rate in particular is not defaulted.
func requireFloat(w http.ResponseWriter, params map[string]string, key, kind string) (float64, bool) {
	raw, present := params[key]
	if !present {
		writeError(w, http.StatusBadRequest, fmt.Sprintf(
			"alt=%s needs %q, and it is not assumed: a rate guessed wrong yields a "+
				"comparison that is well-formed, internally consistent and wrong by a "+
				"few percent, with nothing on the wire to say so", kind, key))
		return 0, false
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf(
			"alt=%s parameter %q must be a number, got %q", kind, key, raw))
		return 0, false
	}
	return v, true
}

func roundCosted(c energy.Costed) energy.Costed {
	c.KWh = round.To(c.KWh, round.KWhDP)
	c.EnergyCost = round.To(c.EnergyCost, round.MoneyDP)
	c.StandingCharge = round.To(c.StandingCharge, round.MoneyDP)
	c.Total = round.To(c.Total, round.MoneyDP)
	return c
}

// roundAlternatives rounds for the wire. Deltas are rounded from their
// full-precision values and the total is re-summed from the rounded components,
// so the reconciliation invariant survives presentation — a consumer checking
// energy + standing == total on the rounded numbers must not find it broken.
func roundAlternatives(alts []energy.Alternative) []energy.Alternative {
	out := make([]energy.Alternative, 0, len(alts))
	for _, a := range alts {
		a.Costed = roundCosted(a.Costed)
		a.Delta.Energy = round.To(a.Delta.Energy, round.MoneyDP)
		a.Delta.Standing = round.To(a.Delta.Standing, round.MoneyDP)
		a.Delta.Total = round.To(a.Delta.Energy+a.Delta.Standing, round.MoneyDP)
		out = append(out, a)
	}
	return out
}
