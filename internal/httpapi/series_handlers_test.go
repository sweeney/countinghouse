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
	Window   string    `json:"window"`
	From     string    `json:"from"`
	To       string    `json:"to"`
	Interval string    `json:"interval"`
	GroupBy  string    `json:"group_by"`
	Buckets  []string  `json:"buckets"`
	Series   []seriesS `json:"series"`
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
	// monitored bucket 0 = winefridge 0.05 + ups 0.1 = 0.15
	if !approx(mon.KWh[0], 0.15) {
		t.Errorf("monitored kwh[0] = %v want 0.15", mon.KWh[0])
	}
	// meter bucket 0 = 0.5
	if !approx(meter.KWh[0], 0.5) {
		t.Errorf("meter kwh[0] = %v want 0.5", meter.KWh[0])
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
