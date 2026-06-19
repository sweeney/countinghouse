package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sweeney/countinghouse/internal/config"
	"github.com/sweeney/countinghouse/internal/influx"
	"github.com/sweeney/countinghouse/internal/round"
)

// seriesResp mirrors the energy.SeriesResponse shape for decoding in tests
// (kept local so the test asserts on the wire JSON, not the Go struct).
type seriesResp struct {
	Window              string    `json:"window"`
	From                string    `json:"from"`
	To                  string    `json:"to"`
	Interval            string    `json:"interval"`
	GroupBy             string    `json:"group_by"`
	Coverage            *float64  `json:"coverage"`
	StaleMonitoredCount *int      `json:"stale_monitored_count"`
	StaleMonitoredIDs   []string  `json:"stale_monitored_ids"`
	Buckets             []string  `json:"buckets"`
	Series              []seriesS `json:"series"`
}

type seriesS struct {
	Key       string    `json:"key"`
	Label     string    `json:"label"`
	Location  string    `json:"location"`
	Class     string    `json:"class"`
	KWh       []float64 `json:"kwh"`
	Cost      []float64 `json:"cost"`
	AvgW      []float64 `json:"avg_w"`
	TotalKWh  float64   `json:"total_kwh"`
	TotalCost float64   `json:"total_cost"`
}

func decodeSeries(t *testing.T, w *httptest.ResponseRecorder) seriesResp {
	t.Helper()
	var r seriesResp
	if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil {
		t.Fatalf("decode series %q: %v", w.Body.String(), err)
	}
	return r
}

// seriesFakeQuerier returns a FakeQuerier whose QueryFunc programs bucketed rows
// keyed on the Flux field and the device set. For the energy_kwh (counter) query
// and the power_w (mean) query it places one row per device per bucket, with the
// row Time on the bucket's right edge (the demux snaps right edges back to the
// containing bucket). per maps device_id → constant value emitted in every
// bucket; field selects which query (energy_kwh vs power_w) the rows answer.
//
// The series builders fan out across a device SET (contains(..., set: [...])),
// so we look at which device ids appear in the flux and emit rows only for those
// that are also present in per.
func seriesFakeQuerier(buckets []time.Time, energyPer, powerPer map[string]float64) *influx.FakeQuerier {
	q := &influx.FakeQuerier{PingOK: true}
	q.QueryFunc = func(flux string) ([]influx.Row, error) {
		isCounter := strings.Contains(flux, `r._field == "energy_kwh"`)
		per := powerPer
		if isCounter {
			per = energyPer
		}
		var rows []influx.Row
		for id, v := range per {
			// Only emit if this device is in the query's device set.
			if !strings.Contains(flux, `"`+id+`"`) {
				continue
			}
			for i := range buckets {
				// Left-edge stamp: the bucket start. demux matches it exactly to
				// the canonical axis (idx[start]).
				rows = append(rows, influx.Row{DeviceID: id, Time: buckets[i], Value: v})
			}
		}
		return rows, nil
	}
	return q
}

// todayHourBuckets returns the canonical 1h bucket axis the dataSetup clock
// produces for window=today: 2026-06-11 00:00 BST .. 14:00 BST (14 buckets).
func todayHourBuckets(t *testing.T) []time.Time {
	t.Helper()
	loc, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Fatal(err)
	}
	var out []time.Time
	start := time.Date(2026, 6, 11, 0, 0, 0, 0, loc)
	for h := 0; h < 14; h++ {
		out = append(out, start.Add(time.Duration(h)*time.Hour))
	}
	return out
}

// seriesSetup is dataSetup with a series-aware FakeQuerier installed.
func seriesSetup(t *testing.T, energyPer, powerPer map[string]float64) *Server {
	t.Helper()
	s, _ := dataSetup(t)
	s.Influx = seriesFakeQuerier(todayHourBuckets(t), energyPer, powerPer)
	return s
}

// TestSeries_RollingWindow exercises the rolling "<N>d" window end-to-end through
// the /series handler: the window label echoes the spec, the interval defaults by
// span (7d → 6h), and From is day-aligned to local midnight 6 days before now
// (clock = 2026-06-11 14:00 BST), distinct from window=today.
func TestSeries_RollingWindow(t *testing.T) {
	s, fake := dataSetup(t)
	fake.PingOK = true // empty result set is fine; we assert on metadata, not values

	w := doGET(t, s, "/series?window=7d&group_by=device")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	r := decodeSeries(t, w)

	if r.Window != "7d" {
		t.Errorf("window = %q want 7d", r.Window)
	}
	if r.Interval != "6h" {
		t.Errorf("interval = %q want 6h (span default)", r.Interval)
	}
	if !strings.HasPrefix(r.From, "2026-06-05T00:00:00") {
		t.Errorf("from = %q want day-aligned 2026-06-05 local midnight", r.From)
	}
	// 7d at 6h spans far more buckets than today's 14×1h — proof it is not today.
	if len(r.Buckets) <= 14 {
		t.Errorf("buckets = %d, want a full week of 6h buckets (>14)", len(r.Buckets))
	}
}

// --- /series group_by=device ---

func TestSeries_Device(t *testing.T) {
	// winefridge (counter) 0.05 kWh/bucket; network-ups (integral) mean 100W;
	// meter 0.5 kWh/bucket but must be EXCLUDED from device grouping.
	energyPer := map[string]float64{"winefridge": 0.05, "electricity_meter": 0.5}
	powerPer := map[string]float64{"winefridge": 52.0, "network-ups": 100.0}
	s := seriesSetup(t, energyPer, powerPer)

	w := doGET(t, s, "/series?window=today&group_by=device")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	r := decodeSeries(t, w)

	if r.Interval != "1h" {
		t.Errorf("interval = %q want 1h", r.Interval)
	}
	if r.GroupBy != "device" {
		t.Errorf("group_by = %q want device", r.GroupBy)
	}
	if len(r.Buckets) != 14 {
		t.Fatalf("buckets = %d want 14", len(r.Buckets))
	}

	// Two metered non-meter devices: winefridge + network-ups. Meter excluded.
	keys := map[string]seriesS{}
	for _, ser := range r.Series {
		keys[ser.Key] = ser
	}
	if _, ok := keys["electricity_meter"]; ok {
		t.Errorf("meter must be excluded from device grouping: %+v", keys)
	}
	if len(r.Series) != 2 {
		t.Fatalf("want 2 series, got %d: %v", len(r.Series), r.Series)
	}

	wf, ok := keys["winefridge"]
	if !ok {
		t.Fatalf("no winefridge series")
	}
	if wf.Label != "Wine Fridge" || wf.Location != "kitchen" {
		t.Errorf("winefridge label/location = %q/%q", wf.Label, wf.Location)
	}
	// Every array aligns to the bucket axis.
	for _, arr := range [][]float64{wf.KWh, wf.Cost, wf.AvgW} {
		if len(arr) != 14 {
			t.Fatalf("array len = %d want 14: %v", len(arr), arr)
		}
	}
	if !approx(wf.KWh[0], 0.05) {
		t.Errorf("winefridge kwh[0] = %v want 0.05", wf.KWh[0])
	}
	if !approx(wf.AvgW[0], 52.0) {
		t.Errorf("winefridge avg_w[0] = %v want 52", wf.AvgW[0])
	}
	// total_kwh = 14 * 0.05 = 0.7
	if !approx(wf.TotalKWh, 0.7) {
		t.Errorf("winefridge total_kwh = %v want 0.7", wf.TotalKWh)
	}
	wantCost0 := round.To(0.05*testUnitRate*(1+testVAT), round.MoneyDP)
	if !approx(wf.Cost[0], wantCost0) {
		t.Errorf("winefridge cost[0] = %v want %v", wf.Cost[0], wantCost0)
	}

	// network-ups: integral path. energy = meanW * hours / 1000 = 100 * 1 / 1000
	// = 0.1 kWh/bucket.
	ups, ok := keys["network-ups"]
	if !ok {
		t.Fatalf("no network-ups series")
	}
	if !approx(ups.KWh[0], 0.1) {
		t.Errorf("ups kwh[0] = %v want 0.1", ups.KWh[0])
	}
}

// --- /series group_by=location ---

func TestSeries_Location(t *testing.T) {
	energyPer := map[string]float64{"winefridge": 0.05}
	powerPer := map[string]float64{"winefridge": 52.0, "network-ups": 100.0}
	s := seriesSetup(t, energyPer, powerPer)

	w := doGET(t, s, "/series?window=today&group_by=location")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	r := decodeSeries(t, w)
	if r.GroupBy != "location" {
		t.Errorf("group_by = %q", r.GroupBy)
	}
	keys := map[string]seriesS{}
	for _, ser := range r.Series {
		keys[ser.Key] = ser
	}
	// kitchen (winefridge) and office (network-ups). meter location excluded.
	if _, ok := keys["kitchen"]; !ok {
		t.Errorf("no kitchen series: %v", keys)
	}
	if _, ok := keys["office"]; !ok {
		t.Errorf("no office series: %v", keys)
	}
	if _, ok := keys["meter"]; ok {
		t.Errorf("meter location must not appear: %v", keys)
	}
}

// --- /series group_by=class ---

func TestSeries_Class(t *testing.T) {
	energyPer := map[string]float64{"winefridge": 0.05}
	powerPer := map[string]float64{"winefridge": 52.0, "network-ups": 100.0}
	s := seriesSetup(t, energyPer, powerPer)

	w := doGET(t, s, "/series?window=today&group_by=class")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	r := decodeSeries(t, w)
	keys := map[string]struct{}{}
	for _, ser := range r.Series {
		keys[ser.Key] = struct{}{}
	}
	if _, ok := keys["continuous_power_device"]; !ok {
		t.Errorf("no continuous_power_device series: %v", keys)
	}
	if _, ok := keys["ups_sensor"]; !ok {
		t.Errorf("no ups_sensor series: %v", keys)
	}
	if _, ok := keys["energy_meter"]; ok {
		t.Errorf("energy_meter class must be excluded: %v", keys)
	}
}

// --- /series group_by=house ---

func TestSeries_House(t *testing.T) {
	energyPer := map[string]float64{"winefridge": 0.05, "electricity_meter": 0.5}
	powerPer := map[string]float64{"winefridge": 52.0, "network-ups": 100.0}
	s := seriesSetup(t, energyPer, powerPer)

	w := doGET(t, s, "/series?window=today&group_by=house")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	r := decodeSeries(t, w)
	keys := map[string]seriesS{}
	for _, ser := range r.Series {
		keys[ser.Key] = ser
	}
	mon, ok := keys["monitored"]
	if !ok {
		t.Fatalf("no monitored series: %v", keys)
	}
	meter, ok := keys["meter"]
	if !ok {
		t.Fatalf("no meter series: %v", keys)
	}
	unmon, ok := keys["unmonitored"]
	if !ok {
		t.Fatalf("no unmonitored series: %v", keys)
	}
	// monitored bucket 0 = winefridge 0.05 + ups 0.1 = 0.15
	if !approx(mon.KWh[0], 0.15) {
		t.Errorf("monitored kwh[0] = %v want 0.15", mon.KWh[0])
	}
	// meter bucket 0 = 0.5
	if !approx(meter.KWh[0], 0.5) {
		t.Errorf("meter kwh[0] = %v want 0.5", meter.KWh[0])
	}
	// unmonitored bucket 0 = meter − monitored = 0.5 − 0.15 = 0.35 (R1.2).
	if !approx(unmon.KWh[0], 0.35) {
		t.Errorf("unmonitored kwh[0] = %v want 0.35", unmon.KWh[0])
	}
	if unmon.Class != "unmonitored" {
		t.Errorf("unmonitored class = %q want unmonitored", unmon.Class)
	}
	// Ordering R1.4: monitored, unmonitored, meter.
	var order []string
	for _, ser := range r.Series {
		order = append(order, ser.Key)
	}
	if len(order) != 3 || order[0] != "monitored" || order[1] != "unmonitored" || order[2] != "meter" {
		t.Errorf("house series order = %v, want [monitored unmonitored meter]", order)
	}
	// Coverage (C12) = monitored ÷ meter for the window; present and consistent
	// with the series totals.
	if r.Coverage == nil {
		t.Fatalf("house response missing coverage")
	}
	if want := round.To(mon.TotalKWh/meter.TotalKWh, 4); !approx(*r.Coverage, want) {
		t.Errorf("coverage = %v want %v (monitored/meter)", *r.Coverage, want)
	}
	// Both monitored devices (winefridge, network-ups) reported power → none stale.
	if r.StaleMonitoredCount == nil || *r.StaleMonitoredCount != 0 {
		t.Errorf("stale_monitored_count = %v want 0", r.StaleMonitoredCount)
	}
}

// TestSeries_HouseStaleness covers C13: a monitored device that reported NO power
// telemetry in the window is flagged stale (its load silently inflates
// unmonitored). Here network-ups has no power samples while winefridge does.
func TestSeries_HouseStaleness(t *testing.T) {
	energyPer := map[string]float64{"winefridge": 0.05, "electricity_meter": 0.5}
	powerPer := map[string]float64{"winefridge": 52.0} // network-ups: no rows → stale
	s := seriesSetup(t, energyPer, powerPer)

	w := doGET(t, s, "/series?window=today&group_by=house")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	r := decodeSeries(t, w)
	if r.StaleMonitoredCount == nil || *r.StaleMonitoredCount != 1 {
		t.Fatalf("stale_monitored_count = %v want 1", r.StaleMonitoredCount)
	}
	if len(r.StaleMonitoredIDs) != 1 || r.StaleMonitoredIDs[0] != "network-ups" {
		t.Errorf("stale_monitored_ids = %v want [network-ups]", r.StaleMonitoredIDs)
	}
}

// Coverage/staleness are house-only signals; a device grouping omits them (C12).
func TestSeries_DeviceGroupingNoHouseStats(t *testing.T) {
	s := seriesSetup(t, map[string]float64{"winefridge": 0.05}, map[string]float64{"winefridge": 52.0})
	w := doGET(t, s, "/series?window=today&group_by=device")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	r := decodeSeries(t, w)
	if r.Coverage != nil || r.StaleMonitoredCount != nil {
		t.Errorf("device grouping should omit house stats: coverage=%v stale=%v", r.Coverage, r.StaleMonitoredCount)
	}
	if strings.Contains(w.Body.String(), "coverage") {
		t.Errorf("coverage key present in device-grouped body: %s", w.Body.String())
	}
}

// --- R3: synthetic "unmonitored" device ---

func TestUnmonitoredDeviceSeries(t *testing.T) {
	energyPer := map[string]float64{"winefridge": 0.05, "electricity_meter": 0.5}
	powerPer := map[string]float64{"winefridge": 52.0, "network-ups": 100.0}
	s := seriesSetup(t, energyPer, powerPer)

	w := doGET(t, s, "/devices/unmonitored/series?window=today")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	r := decodeSeries(t, w)
	// Same schema as a real device series: group_by=device, exactly one series.
	if r.GroupBy != "device" {
		t.Errorf("group_by = %q want device", r.GroupBy)
	}
	if len(r.Series) != 1 || r.Series[0].Key != "unmonitored" {
		t.Fatalf("want one unmonitored series, got %+v", r.Series)
	}
	// Values match the house unmonitored series (R3.2): 0.5 − 0.15 = 0.35.
	if !approx(r.Series[0].KWh[0], 0.35) {
		t.Errorf("unmonitored kwh[0] = %v want 0.35", r.Series[0].KWh[0])
	}
	// avg_w is ENERGY-DERIVED (C8), not 0 — regression for the avg_w=0 bug
	// (docs/bug-unmonitored-avg-w.md). Bucket 0 is a full hour, so
	// avg_w = kwh × 1000 / 1h = 350 W.
	if !approx(r.Series[0].AvgW[0], 350) {
		t.Errorf("unmonitored avg_w[0] = %v want 350 (energy-derived; was 0)", r.Series[0].AvgW[0])
	}
}

func TestUnmonitoredDeviceSeries_NoMeter(t *testing.T) {
	s := seriesSetup(t, nil, nil)
	devs := testDevices()
	delete(devs, "electricity_meter")
	s.Config = fakeConfig{devices: devs, tariffs: testTariffs()}

	w := doGET(t, s, "/devices/unmonitored/series?window=today")
	// No whole-house meter ⇒ unmonitored is undefined, 404 (C6), not monitored.
	if w.Code != http.StatusNotFound {
		t.Fatalf("want 404 with no meter, got %d: %s", w.Code, w.Body.String())
	}
}

// --- R2: include_unmonitored catch-all ---

// countSeries returns how many series carry the given key.
func countSeries(r seriesResp, key string) int {
	n := 0
	for _, s := range r.Series {
		if s.Key == key {
			n++
		}
	}
	return n
}

func sumSeriesTotalKWh(r seriesResp) float64 {
	var sum float64
	for _, s := range r.Series {
		sum += s.TotalKWh
	}
	return sum
}

func TestSeries_IncludeUnmonitored_Device(t *testing.T) {
	energyPer := map[string]float64{"winefridge": 0.05, "electricity_meter": 0.5}
	powerPer := map[string]float64{"winefridge": 52.0, "network-ups": 100.0}
	s := seriesSetup(t, energyPer, powerPer)

	on := decodeSeries(t, doGET(t, s, "/series?window=today&group_by=device&include_unmonitored=true"))
	// Exactly one unmonitored series, tagged synthetic.
	if got := countSeries(on, "unmonitored"); got != 1 {
		t.Fatalf("unmonitored series count = %d, want 1", got)
	}
	var unmon seriesS
	for _, ser := range on.Series {
		if ser.Key == "unmonitored" {
			unmon = ser
		}
	}
	if unmon.Class != "unmonitored" {
		t.Errorf("catch-all class = %q want unmonitored", unmon.Class)
	}

	// Parts sum to the whole house (R2.4): Σ(all series incl. catch-all) == meter.
	house := decodeSeries(t, doGET(t, s, "/series?window=today&group_by=house"))
	var meterTotal, houseUnmon float64
	for _, ser := range house.Series {
		switch ser.Key {
		case "meter":
			meterTotal = ser.TotalKWh
		case "unmonitored":
			houseUnmon = ser.TotalKWh
		}
	}
	if !approx(sumSeriesTotalKWh(on), meterTotal) {
		t.Errorf("Σ device+catch-all = %v, want meter %v", sumSeriesTotalKWh(on), meterTotal)
	}
	// The catch-all is the SAME quantity as the house unmonitored series.
	if !approx(unmon.TotalKWh, houseUnmon) {
		t.Errorf("catch-all total = %v, want house unmonitored %v", unmon.TotalKWh, houseUnmon)
	}
}

// AC6: with location/class grouping the catch-all is never subdivided — exactly
// one unmonitored series regardless of how many groups exist.
func TestSeries_IncludeUnmonitored_ClassNeverSubdivided(t *testing.T) {
	energyPer := map[string]float64{"winefridge": 0.05, "electricity_meter": 0.5}
	powerPer := map[string]float64{"winefridge": 52.0, "network-ups": 100.0}
	s := seriesSetup(t, energyPer, powerPer)

	r := decodeSeries(t, doGET(t, s, "/series?window=today&group_by=class&include_unmonitored=true"))
	if got := countSeries(r, "unmonitored"); got != 1 {
		t.Fatalf("class grouping unmonitored count = %d, want exactly 1", got)
	}
}

// R2.5: without the flag the response is unchanged — no catch-all series.
func TestSeries_IncludeUnmonitored_DefaultOff(t *testing.T) {
	s := seriesSetup(t, map[string]float64{"winefridge": 0.05, "electricity_meter": 0.5}, map[string]float64{"winefridge": 52.0})
	r := decodeSeries(t, doGET(t, s, "/series?window=today&group_by=device"))
	if got := countSeries(r, "unmonitored"); got != 0 {
		t.Errorf("device grouping without flag should have no unmonitored series, got %d", got)
	}
}

// No meter configured ⇒ the catch-all is a graceful no-op (C6), not an error.
func TestSeries_IncludeUnmonitored_NoMeter(t *testing.T) {
	s := seriesSetup(t, map[string]float64{"winefridge": 0.05}, map[string]float64{"winefridge": 52.0})
	devs := testDevices()
	delete(devs, "electricity_meter")
	s.Config = fakeConfig{devices: devs, tariffs: testTariffs()}

	w := doGET(t, s, "/series?window=today&group_by=device&include_unmonitored=true")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	if got := countSeries(decodeSeries(t, w), "unmonitored"); got != 0 {
		t.Errorf("no meter ⇒ no catch-all, got %d unmonitored series", got)
	}
}

func TestSeries_BadIncludeUnmonitored(t *testing.T) {
	s := seriesSetup(t, nil, nil)
	w := doGET(t, s, "/series?window=today&include_unmonitored=maybe")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for bad include_unmonitored, got %d: %s", w.Code, w.Body.String())
	}
}

// --- C3 drift metric + Q4 unclamped mode ---

// driftSetup programs monitored (winefridge 0.5/bucket) to exceed the meter
// (0.3/bucket) so every bucket has a −0.2 kWh residual — drift beyond the 0.1 kWh
// quantum. winefridge reports power so it is not counted stale.
func driftSetup(t *testing.T) *Server {
	return seriesSetup(t,
		map[string]float64{"winefridge": 0.5, "electricity_meter": 0.3},
		map[string]float64{"winefridge": 52.0})
}

func TestSeries_DriftMetric(t *testing.T) {
	s := driftSetup(t)

	if got := decode(t, doGET(t, s, "/metrics"))["drift_buckets_total"].(float64); got != 0 {
		t.Fatalf("drift_buckets_total = %v before any series request, want 0", got)
	}
	if w := doGET(t, s, "/series?window=today&group_by=house"); w.Code != http.StatusOK {
		t.Fatalf("series want 200, got %d: %s", w.Code, w.Body.String())
	}
	if got := decode(t, doGET(t, s, "/metrics"))["drift_buckets_total"].(float64); got <= 0 {
		t.Errorf("drift_buckets_total = %v after drifting house request, want > 0", got)
	}
}

// include_unmonitored on a device grouping also exercises the drift path.
func TestSeries_DriftMetric_CatchAll(t *testing.T) {
	s := driftSetup(t)
	doGET(t, s, "/series?window=today&group_by=device&include_unmonitored=true")
	if got := decode(t, doGET(t, s, "/metrics"))["drift_buckets_total"].(float64); got <= 0 {
		t.Errorf("drift_buckets_total = %v after catch-all request, want > 0", got)
	}
}

// A non-decomposing grouping does not run drift detection.
func TestSeries_NoDriftWithoutDecomposition(t *testing.T) {
	s := driftSetup(t)
	doGET(t, s, "/series?window=today&group_by=device")
	if got := decode(t, doGET(t, s, "/metrics"))["drift_buckets_total"].(float64); got != 0 {
		t.Errorf("drift_buckets_total = %v for plain device grouping, want 0", got)
	}
}

func TestSeries_Unclamped(t *testing.T) {
	s := driftSetup(t)

	// Default (clamped): the unmonitored bucket floors at 0.
	clamped := decodeSeries(t, doGET(t, s, "/series?window=today&group_by=house"))
	for _, ser := range clamped.Series {
		if ser.Key == "unmonitored" && ser.KWh[0] < 0 {
			t.Fatalf("clamped unmonitored kwh[0] = %v, want ≥ 0", ser.KWh[0])
		}
	}

	// unclamped=true: the raw negative residual is preserved.
	w := doGET(t, s, "/series?window=today&group_by=house&unclamped=true")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	var unmon seriesS
	for _, ser := range decodeSeries(t, w).Series {
		if ser.Key == "unmonitored" {
			unmon = ser
		}
	}
	if unmon.Key != "unmonitored" {
		t.Fatalf("no unmonitored series in unclamped house response")
	}
	if unmon.KWh[0] >= 0 {
		t.Errorf("unclamped unmonitored kwh[0] = %v, want negative (meter < monitored)", unmon.KWh[0])
	}
}

func TestSeries_BadUnclamped(t *testing.T) {
	s := seriesSetup(t, nil, nil)
	if w := doGET(t, s, "/series?window=today&unclamped=perhaps"); w.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for bad unclamped, got %d: %s", w.Code, w.Body.String())
	}
}

// The synthetic unmonitored device endpoint also honours unclamped.
func TestUnmonitoredDeviceSeries_Unclamped(t *testing.T) {
	s := driftSetup(t)
	w := doGET(t, s, "/devices/unmonitored/series?window=today&unclamped=true")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	r := decodeSeries(t, w)
	if len(r.Series) != 1 || r.Series[0].KWh[0] >= 0 {
		t.Errorf("unclamped synthetic device kwh[0] should be negative, got %+v", r.Series)
	}
}

// --- interval override / validation ---

func TestSeries_IntervalOverride(t *testing.T) {
	// 15m over today (00:00 .. 14:00 BST) → 56 buckets.
	loc, _ := time.LoadLocation("Europe/London")
	var buckets []time.Time
	start := time.Date(2026, 6, 11, 0, 0, 0, 0, loc)
	for i := 0; i < 56; i++ {
		buckets = append(buckets, start.Add(time.Duration(i)*15*time.Minute))
	}
	s, _ := dataSetup(t)
	s.Influx = seriesFakeQuerier(buckets, map[string]float64{"winefridge": 0.01}, map[string]float64{"winefridge": 20.0})

	w := doGET(t, s, "/series?window=today&interval=15m&group_by=device")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	r := decodeSeries(t, w)
	if r.Interval != "15m" {
		t.Errorf("interval = %q want 15m", r.Interval)
	}
	if len(r.Buckets) != 56 {
		t.Errorf("buckets = %d want 56", len(r.Buckets))
	}
}

func TestSeries_BadInterval(t *testing.T) {
	s := seriesSetup(t, nil, nil)
	w := doGET(t, s, "/series?window=today&interval=7m")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestSeries_IntervalOverCap(t *testing.T) {
	// 5m over a 100-day custom window → ~28800 buckets, over the 1000 cap.
	s := seriesSetup(t, nil, nil)
	w := doGET(t, s, "/series?window=custom&from=2026-01-01T00:00:00Z&to=2026-04-11T00:00:00Z&interval=5m")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400 over cap, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(decode(t, w)["error"].(string), "cap") {
		t.Errorf("error should mention the cap: %v", w.Body.String())
	}
}

func TestSeries_BadGroupBy(t *testing.T) {
	s := seriesSetup(t, nil, nil)
	w := doGET(t, s, "/series?window=today&group_by=galaxy")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestSeries_BadWindow(t *testing.T) {
	s := seriesSetup(t, nil, nil)
	w := doGET(t, s, "/series?window=fortnight")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestSeries_DefaultGroupBy(t *testing.T) {
	s := seriesSetup(t, map[string]float64{"winefridge": 0.05}, map[string]float64{"winefridge": 52.0})
	w := doGET(t, s, "/series?window=today")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	if decodeSeries(t, w).GroupBy != "device" {
		t.Errorf("default group_by should be device")
	}
}

func TestSeries_NoTariff(t *testing.T) {
	s := seriesSetup(t, nil, nil)
	s.Config = fakeConfig{devices: testDevices(), tariffs: config.EnergyTariffs{}}
	w := doGET(t, s, "/series?window=today")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d: %s", w.Code, w.Body.String())
	}
}

func TestSeries_InfluxError(t *testing.T) {
	s := seriesSetup(t, nil, nil)
	s.Influx = &influx.FakeQuerier{PingOK: true, Err: errFake}
	w := doGET(t, s, "/series?window=today")
	if w.Code != http.StatusBadGateway {
		t.Fatalf("want 502, got %d: %s", w.Code, w.Body.String())
	}
}

// --- /devices/{id}/series ---

func TestDeviceSeries_Single(t *testing.T) {
	s := seriesSetup(t, map[string]float64{"winefridge": 0.05}, map[string]float64{"winefridge": 52.0})
	w := doGET(t, s, "/devices/winefridge/series?window=today")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	r := decodeSeries(t, w)
	if len(r.Series) != 1 {
		t.Fatalf("want 1 series, got %d: %v", len(r.Series), r.Series)
	}
	if r.Series[0].Key != "winefridge" {
		t.Errorf("series key = %q want winefridge", r.Series[0].Key)
	}
	if len(r.Buckets) != 14 {
		t.Errorf("buckets = %d want 14", len(r.Buckets))
	}
	if !approx(r.Series[0].KWh[0], 0.05) {
		t.Errorf("kwh[0] = %v want 0.05", r.Series[0].KWh[0])
	}
}

func TestDeviceSeries_IntervalOverride(t *testing.T) {
	loc, _ := time.LoadLocation("Europe/London")
	var buckets []time.Time
	start := time.Date(2026, 6, 11, 0, 0, 0, 0, loc)
	for i := 0; i < 56; i++ {
		buckets = append(buckets, start.Add(time.Duration(i)*15*time.Minute))
	}
	s, _ := dataSetup(t)
	s.Influx = seriesFakeQuerier(buckets, map[string]float64{"winefridge": 0.01}, map[string]float64{"winefridge": 20.0})
	w := doGET(t, s, "/devices/winefridge/series?window=today&interval=15m")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	if decodeSeries(t, w).Interval != "15m" {
		t.Errorf("interval not reflected")
	}
}

func TestDeviceSeries_Unknown(t *testing.T) {
	s := seriesSetup(t, nil, nil)
	w := doGET(t, s, "/devices/nope/series?window=today")
	if w.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d: %s", w.Code, w.Body.String())
	}
}

func TestDeviceSeries_NotMetered(t *testing.T) {
	s := seriesSetup(t, nil, nil)
	w := doGET(t, s, "/devices/hallway-sensor/series?window=today")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", w.Code, w.Body.String())
	}
	if got := decode(t, w)["error"]; got != "device has no energy series" {
		t.Errorf("error = %v", got)
	}
}

func TestDeviceSeries_NoTariff(t *testing.T) {
	s := seriesSetup(t, nil, nil)
	s.Config = fakeConfig{devices: testDevices(), tariffs: config.EnergyTariffs{}}
	w := doGET(t, s, "/devices/winefridge/series?window=today")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d: %s", w.Code, w.Body.String())
	}
}

func TestDeviceSeries_BadInterval(t *testing.T) {
	s := seriesSetup(t, nil, nil)
	w := doGET(t, s, "/devices/winefridge/series?window=today&interval=9m")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", w.Code, w.Body.String())
	}
}

// --- auth still applies to the series routes ---

func TestSeries_RequiresAuth(t *testing.T) {
	priv := genTestKey(t)
	kid := "testkey"
	fakeID := fakeJWKSServer(t, &priv.PublicKey, kid)
	s := seriesSetup(t, nil, nil)
	s.IdentityURL = fakeID.URL
	mux := newMux(s)

	for _, path := range []string{"/series?window=today", "/devices/winefridge/series?window=today"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s no-token: want 401, got %d", path, w.Code)
		}
	}
}
