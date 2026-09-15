package httpapi

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/sweeney/countinghouse/internal/config"
	"github.com/sweeney/countinghouse/internal/energy"
	"github.com/sweeney/countinghouse/internal/prices"
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

	// standingSource says where that figure came from: "archive" when it is the
	// supplier's own inc-VAT daily charge, "config" when it is the configured
	// ex-VAT rate grossed up by the configured VAT rate.
	//
	// Reported on the wire because the two differ in what can go wrong with them.
	// The archived figure is what the supplier bills; the configured one is right
	// only for as long as somebody keeps vat_rate current, which is precisely what
	// a statutory rate change stops being true — see the VAT runbook in README.
	standingSource string

	// flat is the single flat tariff covering the whole window; valid only when
	// scalar is true.
	flat config.Tariff

	// uncovered is set when no agreement covers part of the window, carrying the
	// reason. The plan still prices what it can (nothing, in that stretch) rather than
	// failing, and the caller surfaces this so the gap is visible rather than silent.
	uncovered string

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
	if errors.Is(err, config.ErrNoAgreements) {
		// Nothing configured at all: the service can never price anything, and a
		// deployment in that state wants telling rather than a chart of free
		// electricity. Distinct from a window the agreements simply do not reach.
		return tariffPlan{}, err
	}
	if err != nil {
		// An uncovered stretch is a real state — the agreements document is allowed to
		// describe a period when this house was not a customer — and PeriodsBetween is
		// right to refuse to price it. But refusing to price it is not the same as
		// refusing to ANSWER: the kWh is known and only the money is not.
		//
		// So the window is tiled with a pricer that reports unknown, which is the same
		// treatment a half hour with no archived price already gets. /series renders the
		// energy it has, /bill still reports unpriced_kwh, and neither invents a number.
		// Previously this became a 503 on three series routes, so a chart of data
		// predating the first agreement returned nothing at all — the opposite
		// philosophy to the one this service applies everywhere else.
		return tariffPlan{
			pricer:    energy.SegmentedPricer{Segments: []energy.PricedSegment{{Start: from, Stop: to, Pricer: energy.UnpricedSlots{}}}},
			uncovered: err.Error(),
		}, nil
	}

	plan := tariffPlan{
		standing:       energy.StandingChargeAcross(segments),
		standingSource: standingSourceConfig,
	}

	// Prefer the supplier's own archived standing charge. It is the figure they
	// actually bill, so a VAT change arrives in it rather than having to be applied
	// from configuration — the same reason energy is priced from the archive's
	// inc-VAT column and never grossed up here.
	//
	// Falls back silently to the configured rate, which is what every instance did
	// before standing charges were archived: a deployment with no archive, or a
	// window the archive does not fully cover, is not a reason to refuse a bill.
	if code, ok := singleTariffCode(segments); ok {
		if charged, ok := s.archivedStandingCharge(ctx, code, from, to); ok {
			plan.standing = charged
			plan.standingSource = standingSourceArchive
		}
	}

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
			//
			// UnpricedSlots rather than a nil pricer, because it still DECLARES the
			// half-hour grid. With nil, a coarse display bucket would be judged by its
			// first instant and a partly-held day would read as wholly priced or
			// wholly missing; with this, the energy lands in unpriced_kwh at slot
			// resolution.
			ps.Pricer = energy.UnpricedSlots{}
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
		for i := range billable {
			billable[i].EffectiveRate = billable[i].EffectiveRateOf()
		}
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
		dc.EffectiveRate = dc.EffectiveRateOf()
	}
	return nil
}

// influxFailed writes the 502 the cost handlers share for a failed query.
func influxFailed(w http.ResponseWriter, err error) {
	writeError(w, http.StatusBadGateway, "influx query failed: "+err.Error())
}

// Where a bill's standing charge came from.
const (
	standingSourceArchive = "archive"
	standingSourceConfig  = "config"
)

// singleTariffCode returns the tariff code when EVERY segment shares one.
//
// A window spanning a genuine switchover has two suppliers' standing charges to
// reconcile and two codes to look them up under; that is more than this is worth,
// and config already handles it correctly by segment. A VAT-only split — the case
// this exists for — leaves the code unchanged and so qualifies.
func singleTariffCode(segments []config.Segment) (string, bool) {
	code := ""
	for _, seg := range segments {
		if !seg.Tariff.IsHalfHourly() {
			return "", false
		}
		if code == "" {
			code = seg.Tariff.TariffCode
		} else if code != seg.Tariff.TariffCode {
			return "", false
		}
	}
	return code, code != ""
}

// archivedStandingCharge returns the supplier's own standing charge for the
// window, and whether the archive could answer for all of it.
//
// Partial coverage returns false rather than a partial total: a total missing a
// few days looks like a correct but cheap bill, which is the failure the whole
// pricing layer is built to avoid.
func (s *Server) archivedStandingCharge(ctx context.Context, code string, from, to time.Time) (float64, bool) {
	reader, ok := s.PriceReader.(StandingChargeReader)
	if !ok {
		return 0, false
	}
	charges, err := reader.StandingCharges(ctx, code, from.UTC(), to.UTC())
	if err != nil || len(charges) == 0 {
		// A read failure falls back to config rather than failing the bill. The
		// archive is an improvement on the configured figure, not a dependency of it.
		return 0, false
	}
	return prices.NewSchedule(charges).ChargeOver(from.UTC(), to.UTC())
}
