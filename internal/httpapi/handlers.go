package httpapi

import (
	"net/http"
	"runtime"
	"strconv"
	"time"

	"github.com/sweeney/countinghouse/internal/config"
	"github.com/sweeney/countinghouse/internal/energy"
	"github.com/sweeney/countinghouse/internal/round"
)

// resolveWindowParams parses the window/from/to query params and resolves them
// to a concrete Window using the injected clock + location. It returns a 400
// (written to w) and ok=false on any bad/missing param or unknown window.
//
// window defaults to "today" when absent. For window=custom, from and to are
// required RFC3339 timestamps.
func (s *Server) resolveWindowParams(w http.ResponseWriter, r *http.Request) (energy.Window, bool) {
	q := r.URL.Query()
	spec := q.Get("window")
	if spec == "" {
		spec = energy.WindowToday
	}

	var from, to time.Time
	if v := q.Get("from"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid 'from' (want RFC3339): "+err.Error())
			return energy.Window{}, false
		}
		from = t
	}
	if v := q.Get("to"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid 'to' (want RFC3339): "+err.Error())
			return energy.Window{}, false
		}
		to = t
	}

	// from/to only apply to window=custom; today/week/month are period-to-date
	// and ignore them. Rather than silently discarding a caller-supplied range
	// (and confidently returning a different window's data), reject the
	// contradiction so an intent mismatch surfaces as an actionable 400.
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

// deviceWindowKWh runs energy.DeviceWindowKWh for one device, bumping the
// query counters (and latency) for /metrics.
func (s *Server) deviceWindowKWh(r *http.Request, deviceID, class string, win energy.Window) (kwh float64, source string, err error) {
	start := time.Now()
	kwh, source, err = energy.DeviceWindowKWh(r.Context(), s.Influx, s.Bucket, deviceID, class, win.Start, win.Stop)
	s.queryCount.Add(1)
	s.influxNanos.Add(int64(time.Since(start)))
	if err != nil {
		s.queryErrors.Add(1)
	}
	return kwh, source, err
}

// lookupDevice returns the device config for id, writing a 404 and returning
// ok=false when the id is unknown.
func (s *Server) lookupDevice(w http.ResponseWriter, id string) (config.DeviceConfig, bool) {
	dev, ok := s.Config.Devices()[id]
	if !ok {
		writeError(w, http.StatusNotFound, "unknown device: "+id)
		return config.DeviceConfig{}, false
	}
	return dev, true
}

// handleDeviceEnergy serves GET /devices/{id}/energy.
func (s *Server) handleDeviceEnergy(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	dev, ok := s.lookupDevice(w, id)
	if !ok {
		return
	}
	if _, metered := energy.PathForClass(dev.Class); !metered {
		writeError(w, http.StatusBadRequest, "device has no energy series")
		return
	}
	win, ok := s.resolveWindowParams(w, r)
	if !ok {
		return
	}

	kwh, source, err := s.deviceWindowKWh(r, id, dev.Class, win)
	if err != nil {
		writeError(w, http.StatusBadGateway, "influx query failed: "+err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"device_id": id,
		"kwh":       round.To(kwh, round.KWhDP),
		"source":    source,
		"window":    win.Label,
		"from":      win.Start,
		"to":        win.Stop,
	})
}

// handleDeviceCost serves GET /devices/{id}/cost.
func (s *Server) handleDeviceCost(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	dev, ok := s.lookupDevice(w, id)
	if !ok {
		return
	}
	if _, metered := energy.PathForClass(dev.Class); !metered {
		writeError(w, http.StatusBadRequest, "device has no energy series")
		return
	}
	win, ok := s.resolveWindowParams(w, r)
	if !ok {
		return
	}

	tariff, ok := s.Config.Tariffs().TariffFor(win.Start)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "no electricity tariff configured")
		return
	}

	kwh, _, err := s.deviceWindowKWh(r, id, dev.Class, win)
	if err != nil {
		writeError(w, http.StatusBadGateway, "influx query failed: "+err.Error())
		return
	}

	cost := energy.DeviceCostFor(kwh, tariff)

	writeJSON(w, http.StatusOK, map[string]any{
		"device_id": id,
		"kwh":       round.To(kwh, round.KWhDP),
		"cost":      round.To(cost, round.MoneyDP),
		"currency":  "GBP",
		"window":    win.Label,
		"tariff": map[string]any{
			"unit_rate": tariff.UnitRate,
			"vat_rate":  tariff.VATRate,
		},
	})
}

// resolveSeriesParams resolves the window and interval shared by the series
// handlers. It writes a 400 (and returns ok=false) on a bad window, a bad
// interval, or an interval whose bucket count exceeds the cap. The forWindow
// argument lets the single-device handler validate its interval against the
// resolved window.
func (s *Server) resolveSeriesParams(w http.ResponseWriter, r *http.Request) (energy.Window, energy.Interval, bool) {
	win, ok := s.resolveWindowParams(w, r)
	if !ok {
		return energy.Window{}, energy.Interval{}, false
	}
	iv, err := energy.ResolveInterval(win, r.URL.Query().Get("interval"), s.loc())
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return energy.Window{}, energy.Interval{}, false
	}
	return win, iv, true
}

// buildSeries runs energy.BuildSeries, bumping the query counters (and latency)
// for /metrics. BuildSeries issues up to three Influx queries internally; we
// count the whole series build as one logical query so the per-query latency
// average stays comparable across endpoints.
func (s *Server) buildSeries(r *http.Request, win energy.Window, iv energy.Interval, groupBy string, includeUnmonitored, unclamped bool, devices map[string]config.DeviceConfig, tariff config.Tariff) (energy.SeriesResponse, error) {
	start := time.Now()
	resp, err := energy.BuildSeries(r.Context(), s.Influx, s.Bucket, win, iv, groupBy, includeUnmonitored, unclamped, devices, tariff, s.loc())
	s.queryCount.Add(1)
	s.influxNanos.Add(int64(time.Since(start)))
	if err != nil {
		s.queryErrors.Add(1)
		return resp, err
	}
	s.recordDrift(win, resp.Drift)
	return resp, err
}

// recordDrift turns the operator-facing C3 drift signal into a /metrics counter
// and a WARN log. The counter accumulates flagged buckets across all served
// requests (stateless service: it counts observations, not distinct buckets); the
// log carries the context — window, count, worst residual — for investigation.
func (s *Server) recordDrift(win energy.Window, d energy.DriftStats) {
	if !d.HasDrift() {
		return
	}
	s.driftBuckets.Add(int64(d.ClampedBuckets))
	if s.Logger != nil {
		s.Logger.Warn("unmonitored drift: meter below monitored beyond quantisation",
			"window", win.Label,
			"clamped_buckets", d.ClampedBuckets,
			"worst_residual_kwh", d.WorstResidualKWh,
			"worst_at", d.WorstAt,
		)
	}
}

// validGroupBy reports whether g is an accepted group_by mode.
func validGroupBy(g string) bool {
	switch g {
	case energy.GroupByDevice, energy.GroupByLocation, energy.GroupByClass, energy.GroupByHouse:
		return true
	default:
		return false
	}
}

// handleSeries serves GET /series: a multi-series, columnar energy time-series
// grouped by device (default), location, class, or house.
func (s *Server) handleSeries(w http.ResponseWriter, r *http.Request) {
	groupBy := r.URL.Query().Get("group_by")
	if groupBy == "" {
		groupBy = energy.GroupByDevice
	}
	if !validGroupBy(groupBy) {
		writeError(w, http.StatusBadRequest, "invalid 'group_by' (want one of device, location, class, house)")
		return
	}
	shape := r.URL.Query().Get("shape")
	if !energy.ValidShape(shape) {
		writeError(w, http.StatusBadRequest, "invalid 'shape' (want columns or rows)")
		return
	}
	includeUnmonitored, ok := parseBoolParam(w, r, "include_unmonitored")
	if !ok {
		return
	}
	unclamped, ok := parseBoolParam(w, r, "unclamped")
	if !ok {
		return
	}

	win, iv, ok := s.resolveSeriesParams(w, r)
	if !ok {
		return
	}

	tariff, ok := s.Config.Tariffs().TariffFor(win.Start)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "no electricity tariff configured")
		return
	}

	resp, err := s.buildSeries(r, win, iv, groupBy, includeUnmonitored, unclamped, s.Config.Devices(), tariff)
	if err != nil {
		writeError(w, http.StatusBadGateway, "influx query failed: "+err.Error())
		return
	}
	writeSeriesShaped(w, shape, resp)
}

// parseBoolParam reads an optional boolean query param. Absent ⇒ (false, true):
// the default-off behaviour, byte-for-byte the legacy response. A present value is
// parsed with strconv.ParseBool (true/false/1/0/t/f); anything else writes a 400
// and returns ok=false.
func parseBoolParam(w http.ResponseWriter, r *http.Request, name string) (val bool, ok bool) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return false, true
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid '"+name+"' (want true or false)")
		return false, false
	}
	return v, true
}

// writeSeriesShaped writes a series response in the requested shape: the columnar
// SeriesResponse (default) or, for shape=rows, the row-oriented RowsResponse.
func writeSeriesShaped(w http.ResponseWriter, shape string, resp energy.SeriesResponse) {
	if shape == energy.ShapeRows {
		writeJSON(w, http.StatusOK, resp.Rows())
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleDeviceSeries serves GET /devices/{id}/series: the single-device
// convenience form. It returns the same SeriesResponse shape as /series (so
// consumers can share rendering code) carrying exactly one series for the
// requested device.
func (s *Server) handleDeviceSeries(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	// The reserved synthetic "unmonitored" device is not in the inventory; it is
	// served by deriving the house unmonitored series. Handling it BEFORE
	// lookupDevice also shadows any (disallowed) real device that claims the id.
	if id == energy.UnmonitoredID {
		s.handleUnmonitoredSeries(w, r)
		return
	}
	dev, ok := s.lookupDevice(w, id)
	if !ok {
		return
	}
	if _, metered := energy.PathForClass(dev.Class); !metered {
		writeError(w, http.StatusBadRequest, "device has no energy series")
		return
	}
	shape := r.URL.Query().Get("shape")
	if !energy.ValidShape(shape) {
		writeError(w, http.StatusBadRequest, "invalid 'shape' (want columns or rows)")
		return
	}

	win, iv, ok := s.resolveSeriesParams(w, r)
	if !ok {
		return
	}

	tariff, ok := s.Config.Tariffs().TariffFor(win.Start)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "no electricity tariff configured")
		return
	}

	// Build over a single-device inventory grouped by device, so the response
	// carries exactly this device's series.
	single := map[string]config.DeviceConfig{id: dev}
	resp, err := s.buildSeries(r, win, iv, energy.GroupByDevice, false, false, single, tariff)
	if err != nil {
		writeError(w, http.StatusBadGateway, "influx query failed: "+err.Error())
		return
	}
	writeSeriesShaped(w, shape, resp)
}

// handleUnmonitoredSeries serves GET /devices/unmonitored/series: the synthetic
// rest-of-home device. It returns the SAME response schema as a real device's
// /series (group_by=device, one series) so clients can plot it through their
// existing single-device code path (R3). The values are exactly the house
// grouping's "unmonitored" series (R3.2): we build group_by=house over the full
// inventory and keep only that series. With no whole-house meter configured,
// unmonitored is undefined, so we 404 rather than present monitored as the whole
// home (C6).
func (s *Server) handleUnmonitoredSeries(w http.ResponseWriter, r *http.Request) {
	devices := s.Config.Devices()
	if _, ok := energy.MeterID(devices); !ok {
		writeError(w, http.StatusNotFound, "no whole-house meter configured; unmonitored consumption is undefined")
		return
	}

	shape := r.URL.Query().Get("shape")
	if !energy.ValidShape(shape) {
		writeError(w, http.StatusBadRequest, "invalid 'shape' (want columns or rows)")
		return
	}
	unclamped, ok := parseBoolParam(w, r, "unclamped")
	if !ok {
		return
	}

	win, iv, ok := s.resolveSeriesParams(w, r)
	if !ok {
		return
	}

	tariff, ok := s.Config.Tariffs().TariffFor(win.Start)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "no electricity tariff configured")
		return
	}

	resp, err := s.buildSeries(r, win, iv, energy.GroupByHouse, false, unclamped, devices, tariff)
	if err != nil {
		writeError(w, http.StatusBadGateway, "influx query failed: "+err.Error())
		return
	}

	// Keep only the unmonitored series and present it as a device-shaped response.
	resp.Series = energy.OnlySeries(resp.Series, energy.UnmonitoredID)
	resp.GroupBy = energy.GroupByDevice
	writeSeriesShaped(w, shape, resp)
}

// handleBill serves GET /bill. It queries every billable device (metered,
// excluding the energy meter), separately queries the whole-house meter, and
// assembles the bill + reconciliation.
func (s *Server) handleBill(w http.ResponseWriter, r *http.Request) {
	win, ok := s.resolveWindowParams(w, r)
	if !ok {
		return
	}

	tariff, ok := s.Config.Tariffs().TariffFor(win.Start)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "no electricity tariff configured")
		return
	}

	devices := s.Config.Devices()

	var billable []energy.DeviceCost
	var meterID string
	for id, dev := range devices {
		if _, metered := energy.PathForClass(dev.Class); !metered {
			continue
		}
		if dev.Class == energy.EnergyMeterClass {
			// The whole-house meter is reconciled separately, not billed as a
			// device. Record its id for the meterKWh query below.
			meterID = id
			continue
		}
		billable = append(billable, energy.DeviceCost{
			DeviceID:    id,
			DisplayName: dev.DisplayName,
			Location:    dev.Location,
			Class:       dev.Class,
		})
	}

	for i := range billable {
		dc := &billable[i]
		kwh, _, err := s.deviceWindowKWh(r, dc.DeviceID, dc.Class, win)
		if err != nil {
			writeError(w, http.StatusBadGateway, "influx query failed for "+dc.DeviceID+": "+err.Error())
			return
		}
		dc.KWh = kwh
	}

	// Whole-house meter total. If no electricity meter is configured we pass
	// meterPresent=false so the reconciliation omits the meter-derived fields
	// rather than inventing a misleading negative remainder — don't fail.
	meterPresent := meterID != ""
	var meterKWh float64
	if meterPresent {
		kwh, _, err := s.deviceWindowKWh(r, meterID, energy.EnergyMeterClass, win)
		if err != nil {
			writeError(w, http.StatusBadGateway, "influx query failed for meter "+meterID+": "+err.Error())
			return
		}
		meterKWh = kwh
	}

	bill := energy.AssembleBill(win, billable, meterKWh, meterPresent, tariff)
	writeJSON(w, http.StatusOK, roundBill(bill))
}

// roundBill rounds every numeric field of a Bill for presentation. Totals are
// rounded from their full-precision values (not from the already-rounded parts).
func roundBill(b energy.Bill) energy.Bill {
	for i := range b.Devices {
		b.Devices[i].KWh = round.To(b.Devices[i].KWh, round.KWhDP)
		b.Devices[i].Cost = round.To(b.Devices[i].Cost, round.MoneyDP)
	}
	b.EnergyCost = round.To(b.EnergyCost, round.MoneyDP)
	b.StandingCharge = round.To(b.StandingCharge, round.MoneyDP)
	b.Total = round.To(b.Total, round.MoneyDP)
	b.Reconciliation.MonitoredKWh = round.To(b.Reconciliation.MonitoredKWh, round.KWhDP)
	// The meter-derived fields are nil when no meter is configured; round in
	// place only when present so the "omitted" signal is preserved.
	roundPtr(b.Reconciliation.MeterKWh, round.KWhDP)
	roundPtr(b.Reconciliation.UnmonitoredKWh, round.KWhDP)
	roundPtr(b.Reconciliation.Coverage, round.CovDP)
	return b
}

// roundPtr rounds the float a pointer addresses to dp places, in place. A nil
// pointer (an omitted reconciliation field) is left untouched.
func roundPtr(p *float64, dp int) {
	if p != nil {
		*p = round.To(*p, dp)
	}
}

// handleTariffs serves GET /tariffs, returning all configured tariffs keyed by
// fuel (electricity, gas, ...). Countinghouse only bills electricity today, but
// exposing the full set keeps the API forward-compatible with gas devices.
func (s *Server) handleTariffs(w http.ResponseWriter, _ *http.Request) {
	tariffs := s.Config.Tariffs()
	if len(tariffs.Tariffs) == 0 {
		writeError(w, http.StatusNotFound, "no tariffs configured")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"currency": "GBP",
		"tariffs":  tariffs.Tariffs,
	})
}

// handleMetrics serves GET /metrics: atomic query counters plus runtime info.
func (s *Server) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	count := s.queryCount.Load()
	var avgMs float64
	if count > 0 {
		avgMs = float64(s.influxNanos.Load()) / float64(count) / 1e6
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"query_count":           count,
		"query_errors":          s.queryErrors.Load(),
		"influx_avg_latency_ms": round.To(avgMs, 2),
		"drift_buckets_total":   s.driftBuckets.Load(),
		"version":               s.Version,
		"uptime_seconds":        int(time.Since(s.started) / time.Second),
		"goroutines":            runtime.NumGoroutine(),
	})
}

// writeError writes a JSON error body with the given status.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
