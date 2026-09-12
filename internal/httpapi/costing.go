package httpapi

import (
	"context"
	"net/http"
	"time"

	"github.com/sweeney/countinghouse/internal/config"
	"github.com/sweeney/countinghouse/internal/energy"
)

// ---------------------------------------------------------------------------
// How a window gets costed.
//
// Every cost in this service used to be `kWh x tariff.UnitRate x (1 + vat)`: one
// whole-window energy figure multiplied by one number from config. A half-hourly
// tariff breaks both halves of that. There is no single rate — there are
// forty-eight a day, of either sign — and the config rate is deliberately ZERO,
// because the price lives in the archive. The multiplication therefore did not
// fail on such a tariff. It returned £0.00 for real consumption, which is the
// worst available outcome: a wrong answer wearing the shape of a right one.
//
// So the cost path now asks two questions in order:
//
//	1. What prices the energy in this window?   -> tariffPlan.pricer
//	2. At what resolution must energy be measured to apply them?
//
//	   one flat tariff covers the window          many rates, or a switchover
//	   ------------------------------------        ----------------------------
//	   one increase() over the whole window        per-half-hour counter deltas
//	   x one rate                                  each x its own slot's rate
//	   (exact; unchanged; one query)               (decision C1; 48 buckets/day)
//
// The flat branch survives because it is exact AND cheap, and because a month's
// bill on a flat tariff should not start issuing 1488-bucket queries to reach the
// same answer it reaches with one.
// ---------------------------------------------------------------------------

// tariffPlan is everything the cost path needs from the tariff side of a window:
// how to price any instant in it, what the standing charge comes to, and whether
// the cheap whole-window query is still exact.
type tariffPlan struct {
	// pricer answers the VAT-inclusive £/kWh at any instant in the window.
	pricer energy.Pricer

	// standing is the VAT-inclusive £ standing charge for the window, apportioned
	// across tariff segments so a window spanning a switchover charges each side's
	// daily rate for its own days.
	standing float64

	// flat is the single flat tariff covering the whole window; valid only when
	// scalar is true.
	flat config.Tariff

	// scalar reports that ONE flat tariff covers the whole window, which is the
	// only case where a single whole-window energy figure times a single rate is
	// exact. Two flat tariffs either side of a switchover do not qualify: the
	// window's energy would have to be split at the boundary anyway, and the
	// bucketed path already does that correctly.
	scalar bool
}

// attribution names the method this plan implies, for the wire.
func (p tariffPlan) attribution() string {
	if p.scalar {
		return energy.AttributionFlatRate
	}
	return energy.AttributionCounterSlot
}

// planFor resolves the pricing for [from, to).
//
// Three things it has to get right at once: a window may span a tariff change (the
// first Agile bill necessarily does), each side carries its own VAT rate, and a
// half-hourly segment's rate comes from the archive per half hour rather than from
// config at all.
//
// A segment whose prices are not held does not fail the request. It yields a pricer
// that reports unknown, and the energy in it surfaces as unpriced_kwh — visible,
// rather than either a 500 or a silent £0.00.
func (s *Server) planFor(ctx context.Context, from, to time.Time) (tariffPlan, error) {
	segments, err := s.Config.Tariffs().PeriodsBetween(from, to)
	if err != nil {
		return tariffPlan{}, err
	}

	plan := tariffPlan{standing: energy.StandingChargeAcross(segments)}

	priced := make([]energy.PricedSegment, 0, len(segments))
	var halfHourly bool
	for _, seg := range segments {
		ps := energy.PricedSegment{Start: seg.Start, Stop: seg.Stop}
		if !seg.Tariff.IsHalfHourly() {
			ps.Pricer = energy.FlatPricerFor(seg.Tariff)
			priced = append(priced, ps)
			continue
		}
		halfHourly = true
		if s.PriceReader == nil {
			// No archive on this instance: leave the segment unpriced rather than
			// refusing the whole window. A flat segment either side still bills, and
			// the gap is reported rather than charged at nothing.
			priced = append(priced, ps)
			continue
		}
		curve, err := s.curveFor(ctx, seg.Tariff.TariffCode, seg.Start.UTC(), seg.Stop.UTC())
		if err != nil {
			return tariffPlan{}, err
		}
		ps.Pricer = curve
		priced = append(priced, ps)
	}

	plan.pricer = energy.SegmentedPricer{Segments: priced}
	if len(segments) == 1 && !halfHourly {
		plan.flat, plan.scalar = segments[0].Tariff, true
	}
	return plan, nil
}

// pricerFor is planFor for callers that need only the pricer — the series
// handlers, which take their energy from buckets they have already chosen.
func (s *Server) pricerFor(ctx context.Context, from, to time.Time) (energy.Pricer, error) {
	plan, err := s.planFor(ctx, from, to)
	if err != nil {
		return nil, err
	}
	return plan.pricer, nil
}

// deviceTotals is one device's costed energy for a window.
type deviceTotals struct {
	kwh      float64
	cost     float64
	unpriced float64
}

// slotCosts prices a device set per half hour, returning each device's totals.
//
// It runs the same build the /series endpoint runs — same query, same bucketing,
// same pricer — and sums the result. That is deliberate: /series and /bill
// answering the same question two ways is how they come to disagree, and a
// consumer comparing a chart against a bill has no way to tell which one lied.
//
// The interval is energy.CostingInterval(), which bypasses the MaxBuckets cap. The
// cap guards RESPONSE SIZE for charts; here the buckets are summed to a handful of
// scalars and never reach the wire, so enforcing it would only make a month's bill
// unanswerable.
func (s *Server) slotCosts(r *http.Request, win energy.Window, devices map[string]config.DeviceConfig, pricer energy.Pricer) (map[string]deviceTotals, error) {
	resp, err := s.buildSeries(r, win, energy.CostingInterval(), energy.GroupByDevice,
		false /* includeUnmonitored: the bill reconciles the meter separately */, false, devices, pricer)
	if err != nil {
		return nil, err
	}
	out := make(map[string]deviceTotals, len(resp.Series))
	for _, ser := range resp.Series {
		out[ser.Key] = deviceTotals{kwh: ser.TotalKWh, cost: ser.TotalCost, unpriced: ser.UnpricedKWh}
	}
	return out, nil
}

// costDevices fills in KWh, Cost and UnpricedKWh for each billable device under
// the given plan, picking the scalar or the bucketed path.
//
// devices must be the inventory the costs are drawn from, and billable the subset
// to report — the two differ for /bill, where the whole-house meter is in the
// inventory but is reconciled rather than billed.
func (s *Server) costDevices(r *http.Request, win energy.Window, plan tariffPlan, devices map[string]config.DeviceConfig, billable []energy.DeviceCost) error {
	if plan.scalar {
		for i := range billable {
			dc := &billable[i]
			kwh, _, err := s.deviceWindowKWh(r, dc.DeviceID, dc.Class, win)
			if err != nil {
				return err
			}
			dc.KWh = kwh
		}
		energy.PriceFlat(billable, plan.flat)
		return nil
	}

	totals, err := s.slotCosts(r, win, devices, plan.pricer)
	if err != nil {
		return err
	}
	for i := range billable {
		dc := &billable[i]
		// A device absent from the build has no readings in the window, which is
		// zero energy and zero cost — not an error, and not a reason to fail a bill
		// that the other devices can answer.
		t := totals[dc.DeviceID]
		dc.KWh, dc.Cost, dc.UnpricedKWh = t.kwh, t.cost, t.unpriced
	}
	return nil
}

// influxFailed writes the 502 the cost handlers share for a failed query.
func influxFailed(w http.ResponseWriter, err error) {
	writeError(w, http.StatusBadGateway, "influx query failed: "+err.Error())
}
