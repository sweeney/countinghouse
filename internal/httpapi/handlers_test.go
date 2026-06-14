package httpapi

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sweeney/countinghouse/internal/config"
	"github.com/sweeney/countinghouse/internal/influx"
	"github.com/sweeney/countinghouse/internal/testutil"
)

// fakeConfig is a static ConfigProvider for handler tests.
type fakeConfig struct {
	devices map[string]config.DeviceConfig
	tariffs config.EnergyTariffs
}

func (f fakeConfig) Devices() map[string]config.DeviceConfig { return f.devices }
func (f fakeConfig) Tariffs() config.EnergyTariffs           { return f.tariffs }

const (
	testUnitRate = 0.2089
	testStanding = 0.5294
	testVAT      = 0.05
)

func testTariffs() config.EnergyTariffs {
	return config.EnergyTariffs{Tariffs: map[string]config.Tariff{
		"electricity": {
			UnitRate:            testUnitRate,
			DailyStandingCharge: testStanding,
			Unit:                "kWh",
			VATRate:             testVAT,
		},
		"gas": {
			UnitRate:            0.0502,
			DailyStandingCharge: 0.317,
			Unit:                "kWh",
			VATRate:             testVAT,
		},
	}}
}

func testDevices() map[string]config.DeviceConfig {
	return map[string]config.DeviceConfig{
		"winefridge": {
			Class: "continuous_power_device", Location: "kitchen", DisplayName: "Wine Fridge",
		},
		"network-ups": {
			Class: "ups_sensor", Location: "office", DisplayName: "Network UPS",
		},
		"electricity_meter": {
			Class: "energy_meter", Location: "meter", DisplayName: "Electricity Meter",
		},
		"hallway-sensor": {
			Class: "environmental_sensor", Location: "hall", DisplayName: "Hall Sensor",
		},
		"hot_water": {
			Class: "binary_state_device", Location: "utility", DisplayName: "Hot Water",
		},
		"boiler": {
			Class: "binary_state_device", Location: "utility", DisplayName: "Boiler",
		},
	}
}

// dataSetup returns a Server wired with a FakeQuerier (keyed responses), a
// FakeClock fixed to a known instant, Loc Europe/London, and a fakeConfig.
func dataSetup(t *testing.T) (*Server, *influx.FakeQuerier) {
	t.Helper()
	loc, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Fatalf("load location: %v", err)
	}
	q := &influx.FakeQuerier{
		PingOK: true,
		Responses: map[string][]influx.Row{
			`r.device_id == "winefridge"`:        {{Value: 3.0}},
			`r.device_id == "network-ups"`:       {{Value: 1.5}},
			`r.device_id == "electricity_meter"`: {{Value: 10.0}},
		},
	}
	s := New(":0", q, nil)
	s.Bucket = "statehouse"
	// Fixed instant: 2026-06-11 14:00:00 BST = 13:00 UTC.
	s.Clock = testutil.NewFakeClock(time.Date(2026, 6, 11, 13, 0, 0, 0, time.UTC))
	s.Loc = loc
	s.Config = fakeConfig{devices: testDevices(), tariffs: testTariffs()}
	return s, q
}

func doGET(t *testing.T, s *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	w := httptest.NewRecorder()
	s.handler().ServeHTTP(w, req)
	return w
}

func decode(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode body %q: %v", w.Body.String(), err)
	}
	return m
}

func approx(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

// --- /devices/{id}/energy ---

func TestDeviceEnergy_Counter(t *testing.T) {
	s, _ := dataSetup(t)
	w := doGET(t, s, "/devices/winefridge/energy?window=today")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	m := decode(t, w)
	if m["device_id"] != "winefridge" {
		t.Errorf("device_id = %v", m["device_id"])
	}
	if !approx(m["kwh"].(float64), 3.0) {
		t.Errorf("kwh = %v want 3.0", m["kwh"])
	}
	if m["source"] != "counter" {
		t.Errorf("source = %v want counter", m["source"])
	}
	if m["window"] != "today" {
		t.Errorf("window = %v", m["window"])
	}
	if m["from"] == nil || m["to"] == nil {
		t.Errorf("missing from/to: %v", m)
	}
}

func TestDeviceEnergy_Integral(t *testing.T) {
	s, _ := dataSetup(t)
	w := doGET(t, s, "/devices/network-ups/energy")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	m := decode(t, w)
	if m["source"] != "integral" {
		t.Errorf("source = %v want integral", m["source"])
	}
	if !approx(m["kwh"].(float64), 1.5) {
		t.Errorf("kwh = %v want 1.5", m["kwh"])
	}
}

func TestDeviceEnergy_UnknownDevice(t *testing.T) {
	s, _ := dataSetup(t)
	w := doGET(t, s, "/devices/nope/energy")
	if w.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d: %s", w.Code, w.Body.String())
	}
}

func TestDeviceEnergy_NotMetered(t *testing.T) {
	s, _ := dataSetup(t)
	w := doGET(t, s, "/devices/hallway-sensor/energy")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", w.Code, w.Body.String())
	}
	if got := decode(t, w)["error"]; got != "device has no energy series" {
		t.Errorf("error = %v", got)
	}
}

func TestDeviceEnergy_BadWindow(t *testing.T) {
	s, _ := dataSetup(t)
	w := doGET(t, s, "/devices/winefridge/energy?window=fortnight")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestDeviceEnergy_Custom(t *testing.T) {
	s, _ := dataSetup(t)
	w := doGET(t, s, "/devices/winefridge/energy?window=custom&from=2026-06-01T00:00:00Z&to=2026-06-08T00:00:00Z")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	m := decode(t, w)
	if m["window"] != "custom" {
		t.Errorf("window = %v want custom", m["window"])
	}
}

// TestWindow_FromToOnlyValidWithCustom locks issue #9: supplying from/to with a
// non-custom window (today/week/month) is a contradiction — those windows are
// period-to-date and ignore from/to — so it must be a 400 with an explanatory
// message rather than silently discarding the caller-supplied range and
// returning a different window's data.
func TestWindow_FromToOnlyValidWithCustom(t *testing.T) {
	cases := []string{
		"/devices/winefridge/energy?window=today&from=2026-01-01T00:00:00Z&to=2026-02-01T00:00:00Z",
		"/devices/winefridge/energy?window=week&from=2026-01-01T00:00:00Z",
		"/devices/winefridge/energy?window=month&to=2026-02-01T00:00:00Z",
		// window omitted defaults to today, so a stray from is still contradictory.
		"/devices/winefridge/energy?from=2026-01-01T00:00:00Z",
	}
	for _, path := range cases {
		w := doGET(t, dataSetupT(t), path)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: want 400, got %d: %s", path, w.Code, w.Body.String())
			continue
		}
		if got := decode(t, w)["error"]; got != "'from'/'to' are only valid with window=custom" {
			t.Errorf("%s: error = %v", path, got)
		}
	}
}

// dataSetupT is a one-value wrapper around dataSetup for table tests.
func dataSetupT(t *testing.T) *Server {
	t.Helper()
	s, _ := dataSetup(t)
	return s
}

func TestDeviceEnergy_CustomMissingTo(t *testing.T) {
	s, _ := dataSetup(t)
	w := doGET(t, s, "/devices/winefridge/energy?window=custom&from=2026-06-01T00:00:00Z")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestDeviceEnergy_BadFrom(t *testing.T) {
	s, _ := dataSetup(t)
	w := doGET(t, s, "/devices/winefridge/energy?window=custom&from=not-a-time&to=2026-06-08T00:00:00Z")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", w.Code, w.Body.String())
	}
}

// --- /devices/{id}/cost ---

func TestDeviceCost(t *testing.T) {
	s, _ := dataSetup(t)
	w := doGET(t, s, "/devices/winefridge/cost?window=today")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	m := decode(t, w)
	want := roundTo(3.0*testUnitRate*(1+testVAT), moneyDP)
	if !approx(m["cost"].(float64), want) {
		t.Errorf("cost = %v want %v", m["cost"], want)
	}
	if m["currency"] != "GBP" {
		t.Errorf("currency = %v", m["currency"])
	}
	tariff := m["tariff"].(map[string]any)
	if !approx(tariff["unit_rate"].(float64), testUnitRate) {
		t.Errorf("tariff.unit_rate = %v", tariff["unit_rate"])
	}
	if !approx(tariff["vat_rate"].(float64), testVAT) {
		t.Errorf("tariff.vat_rate = %v", tariff["vat_rate"])
	}
}

func TestDeviceCost_NoTariff(t *testing.T) {
	s, _ := dataSetup(t)
	s.Config = fakeConfig{devices: testDevices(), tariffs: config.EnergyTariffs{}}
	w := doGET(t, s, "/devices/winefridge/cost")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d: %s", w.Code, w.Body.String())
	}
}

// --- /bill ---

func TestBill(t *testing.T) {
	s, _ := dataSetup(t)
	w := doGET(t, s, "/bill?window=month")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}

	var bill struct {
		Window   string `json:"window"`
		Currency string `json:"currency"`
		Devices  []struct {
			DeviceID string  `json:"device_id"`
			KWh      float64 `json:"kwh"`
			Cost     float64 `json:"cost"`
		} `json:"devices"`
		EnergyCost     float64 `json:"energy_cost"`
		StandingCharge float64 `json:"standing_charge"`
		Total          float64 `json:"total"`
		Reconciliation struct {
			MonitoredKWh   float64 `json:"monitored_kwh"`
			MeterKWh       float64 `json:"meter_kwh"`
			UnmonitoredKWh float64 `json:"unmonitored_kwh"`
			Coverage       float64 `json:"coverage"`
		} `json:"reconciliation"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &bill); err != nil {
		t.Fatalf("decode bill: %v", err)
	}

	// Meter excluded from devices; non-metered excluded; winefridge + ups in.
	if len(bill.Devices) != 2 {
		t.Fatalf("want 2 billable devices, got %d: %+v", len(bill.Devices), bill.Devices)
	}
	for _, d := range bill.Devices {
		if d.DeviceID == "electricity_meter" || d.DeviceID == "hallway-sensor" {
			t.Errorf("device %s should not be billable", d.DeviceID)
		}
	}

	monitored := 3.0 + 1.5
	if !approx(bill.Reconciliation.MonitoredKWh, monitored) {
		t.Errorf("monitored_kwh = %v want %v", bill.Reconciliation.MonitoredKWh, monitored)
	}
	if !approx(bill.Reconciliation.MeterKWh, 10.0) {
		t.Errorf("meter_kwh = %v want 10", bill.Reconciliation.MeterKWh)
	}
	if !approx(bill.Reconciliation.UnmonitoredKWh, 10.0-monitored) {
		t.Errorf("unmonitored_kwh = %v want %v", bill.Reconciliation.UnmonitoredKWh, 10.0-monitored)
	}
	if !approx(bill.Reconciliation.Coverage, monitored/10.0) {
		t.Errorf("coverage = %v want %v", bill.Reconciliation.Coverage, monitored/10.0)
	}

	rawEnergy := monitored * testUnitRate * (1 + testVAT)
	if !approx(bill.EnergyCost, roundTo(rawEnergy, moneyDP)) {
		t.Errorf("energy_cost = %v want %v", bill.EnergyCost, roundTo(rawEnergy, moneyDP))
	}

	// Standing charge: window.Days() for month-to-date. now=2026-06-11 13:00 UTC,
	// month start = 2026-06-01 00:00 BST = 2026-05-31 23:00 UTC. Days = elapsed/24h.
	loc, _ := time.LoadLocation("Europe/London")
	start := time.Date(2026, 6, 1, 0, 0, 0, 0, loc)
	stop := time.Date(2026, 6, 11, 13, 0, 0, 0, time.UTC)
	wantDays := stop.Sub(start).Hours() / 24
	rawStanding := wantDays * testStanding * (1 + testVAT)
	if !approx(bill.StandingCharge, roundTo(rawStanding, moneyDP)) {
		t.Errorf("standing_charge = %v want %v", bill.StandingCharge, roundTo(rawStanding, moneyDP))
	}
	// Total is rounded from the full-precision sum, not the rounded parts.
	if !approx(bill.Total, roundTo(rawEnergy+rawStanding, moneyDP)) {
		t.Errorf("total = %v want %v", bill.Total, roundTo(rawEnergy+rawStanding, moneyDP))
	}
}

func TestBill_NoMeter(t *testing.T) {
	s, _ := dataSetup(t)
	devs := testDevices()
	delete(devs, "electricity_meter")
	s.Config = fakeConfig{devices: devs, tariffs: testTariffs()}
	w := doGET(t, s, "/bill?window=month")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	var bill struct {
		Reconciliation struct {
			MeterKWh float64 `json:"meter_kwh"`
			Coverage float64 `json:"coverage"`
		} `json:"reconciliation"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &bill); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if bill.Reconciliation.MeterKWh != 0 || bill.Reconciliation.Coverage != 0 {
		t.Errorf("want meter 0 / coverage 0, got %+v", bill.Reconciliation)
	}
}

// --- /tariffs ---

func TestTariffs(t *testing.T) {
	s, _ := dataSetup(t)
	w := doGET(t, s, "/tariffs")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	m := decode(t, w)
	if m["currency"] != "GBP" {
		t.Errorf("currency = %v want GBP", m["currency"])
	}
	tariffs, ok := m["tariffs"].(map[string]any)
	if !ok {
		t.Fatalf("tariffs not an object: %v", m)
	}
	// Split by fuel: electricity and gas both present.
	elec, ok := tariffs["electricity"].(map[string]any)
	if !ok {
		t.Fatalf("no electricity tariff: %v", tariffs)
	}
	if !approx(elec["unit_rate"].(float64), testUnitRate) {
		t.Errorf("electricity.unit_rate = %v", elec["unit_rate"])
	}
	if !approx(elec["daily_standing_charge"].(float64), testStanding) {
		t.Errorf("electricity.daily_standing_charge = %v", elec["daily_standing_charge"])
	}
	if _, ok := tariffs["gas"].(map[string]any); !ok {
		t.Errorf("expected gas tariff present for extensibility: %v", tariffs)
	}
}

// TestDeviceEnergy_RoundsFloatTail locks the response-boundary rounding: a noisy
// Influx value (long float tail) must come back tidied to kwhDP places.
func TestDeviceEnergy_RoundsFloatTail(t *testing.T) {
	s, q := dataSetup(t)
	q.Responses[`r.device_id == "winefridge"`] = []influx.Row{{Value: 1.1000000000000085}}
	w := doGET(t, s, "/devices/winefridge/energy?window=today")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	if got := decode(t, w)["kwh"].(float64); got != 1.1 {
		t.Errorf("kwh = %v want 1.1 (rounded)", got)
	}
}

func TestTariffs_Absent(t *testing.T) {
	s, _ := dataSetup(t)
	s.Config = fakeConfig{devices: testDevices(), tariffs: config.EnergyTariffs{}}
	w := doGET(t, s, "/tariffs")
	if w.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d: %s", w.Code, w.Body.String())
	}
}

// --- /metrics ---

func TestMetrics_QueryCountIncrements(t *testing.T) {
	s, _ := dataSetup(t)

	// Baseline.
	w := doGET(t, s, "/metrics")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	before := decode(t, w)["query_count"].(float64)

	// One data query.
	if w := doGET(t, s, "/devices/winefridge/energy"); w.Code != http.StatusOK {
		t.Fatalf("energy query failed: %d", w.Code)
	}

	w = doGET(t, s, "/metrics")
	after := decode(t, w)["query_count"].(float64)
	if after != before+1 {
		t.Errorf("query_count = %v, want %v", after, before+1)
	}
}

func TestMetrics_QueryErrors(t *testing.T) {
	s, q := dataSetup(t)
	q.Err = errFake
	if w := doGET(t, s, "/devices/winefridge/energy"); w.Code != http.StatusBadGateway {
		t.Fatalf("want 502 on influx error, got %d", w.Code)
	}
	q.Err = nil
	w := doGET(t, s, "/metrics")
	if decode(t, w)["query_errors"].(float64) < 1 {
		t.Errorf("query_errors not incremented: %v", w.Body.String())
	}
}

var errFake = &fakeErr{}

type fakeErr struct{}

func (*fakeErr) Error() string { return "boom" }
