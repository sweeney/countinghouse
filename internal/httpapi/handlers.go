package httpapi

import (
	"math"
	"net/http"
	"runtime"
	"time"

	"github.com/sweeney/countinghouse/internal/config"
	"github.com/sweeney/countinghouse/internal/energy"
)

// Decimal places for rounding numbers in API responses. Influx increase()/
// integral() and the cost multiply produce long float tails (e.g.
// 0.24127950000000187); we round at the response boundary so consumers see
// tidy values. kWh to 3 dp (~Wh), money to 4 dp (sub-penny, keeps tiny
// per-device costs meaningful), coverage to 4 dp.
const (
	kwhDP   = 3
	moneyDP = 4
	covDP   = 4
)

// roundTo rounds f to dp decimal places, passing NaN/Inf through untouched.
func roundTo(f float64, dp int) float64 {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return f
	}
	p := math.Pow(10, float64(dp))
	return math.Round(f*p) / p
}

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
		"kwh":       roundTo(kwh, kwhDP),
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
		"kwh":       roundTo(kwh, kwhDP),
		"cost":      roundTo(cost, moneyDP),
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
func (s *Server) buildSeries(r *http.Request, win energy.Window, iv energy.Interval, groupBy string, devices map[string]config.DeviceConfig, tariff config.Tariff) (energy.SeriesResponse, error) {
	start := time.Now()
	resp, err := energy.BuildSeries(r.Context(), s.Influx, s.Bucket, win, iv, groupBy, devices, tariff, s.loc())
	s.queryCount.Add(1)
	s.influxNanos.Add(int64(time.Since(start)))
	if err != nil {
		s.queryErrors.Add(1)
	}
	return resp, err
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

	win, iv, ok := s.resolveSeriesParams(w, r)
	if !ok {
		return
	}

	tariff, ok := s.Config.Tariffs().TariffFor(win.Start)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "no electricity tariff configured")
		return
	}

	resp, err := s.buildSeries(r, win, iv, groupBy, s.Config.Devices(), tariff)
	if err != nil {
		writeError(w, http.StatusBadGateway, "influx query failed: "+err.Error())
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
	dev, ok := s.lookupDevice(w, id)
	if !ok {
		return
	}
	if _, metered := energy.PathForClass(dev.Class); !metered {
		writeError(w, http.StatusBadRequest, "device has no energy series")
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
	resp, err := s.buildSeries(r, win, iv, energy.GroupByDevice, single, tariff)
	if err != nil {
		writeError(w, http.StatusBadGateway, "influx query failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, resp)
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
		if dev.Class == "energy_meter" {
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

	// Whole-house meter total. If no electricity meter is configured, meterKWh
	// stays 0 (reconciliation then shows coverage 0) — don't fail.
	var meterKWh float64
	if meterID != "" {
		kwh, _, err := s.deviceWindowKWh(r, meterID, "energy_meter", win)
		if err != nil {
			writeError(w, http.StatusBadGateway, "influx query failed for meter "+meterID+": "+err.Error())
			return
		}
		meterKWh = kwh
	}

	bill := energy.AssembleBill(win, billable, meterKWh, tariff)
	writeJSON(w, http.StatusOK, roundBill(bill))
}

// roundBill rounds every numeric field of a Bill for presentation. Totals are
// rounded from their full-precision values (not from the already-rounded parts).
func roundBill(b energy.Bill) energy.Bill {
	for i := range b.Devices {
		b.Devices[i].KWh = roundTo(b.Devices[i].KWh, kwhDP)
		b.Devices[i].Cost = roundTo(b.Devices[i].Cost, moneyDP)
	}
	b.EnergyCost = roundTo(b.EnergyCost, moneyDP)
	b.StandingCharge = roundTo(b.StandingCharge, moneyDP)
	b.Total = roundTo(b.Total, moneyDP)
	b.Reconciliation.MonitoredKWh = roundTo(b.Reconciliation.MonitoredKWh, kwhDP)
	b.Reconciliation.MeterKWh = roundTo(b.Reconciliation.MeterKWh, kwhDP)
	b.Reconciliation.UnmonitoredKWh = roundTo(b.Reconciliation.UnmonitoredKWh, kwhDP)
	b.Reconciliation.Coverage = roundTo(b.Reconciliation.Coverage, covDP)
	return b
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
		"influx_avg_latency_ms": roundTo(avgMs, 2),
		"version":               s.Version,
		"uptime_seconds":        int(time.Since(s.started) / time.Second),
		"goroutines":            runtime.NumGoroutine(),
	})
}

// writeError writes a JSON error body with the given status.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
