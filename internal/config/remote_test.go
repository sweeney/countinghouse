package config

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// staticTokenSource satisfies TokenSource for tests.
type staticTokenSource struct{ token string }

func (s *staticTokenSource) Token(_ context.Context) (string, error) { return s.token, nil }
func (s *staticTokenSource) Invalidate()                             {}

// trackingTokenSource records whether Invalidate was called.
type trackingTokenSource struct {
	token       string
	invalidated bool
}

func (t *trackingTokenSource) Token(_ context.Context) (string, error) { return t.token, nil }
func (t *trackingTokenSource) Invalidate()                             { t.invalidated = true }

func newTestFetcher(t *testing.T, mux *http.ServeMux, tokens TokenSource) *Fetcher {
	t.Helper()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &Fetcher{
		BaseURL:    srv.URL,
		Tokens:     tokens,
		HTTPClient: srv.Client(),
	}
}

// serveNamespace serves a JSON namespace requiring Bearer test-token.
func serveNamespace(mux *http.ServeMux, ns string, v any) {
	mux.HandleFunc("/api/v1/config/"+ns, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(v)
	})
}

func TestFetcher_RefreshPopulatesSnapshots(t *testing.T) {
	mux := http.NewServeMux()
	serveNamespace(mux, "statehouse_devices", map[string]any{
		"washingmachine": map[string]any{
			// Legacy Z2M shorthand: normaliseDevices folds these into
			// scheme=zigbee, primary=ieee_address, display=friendly_name.
			"ieee_address":  "0xaabbccddeeff0011",
			"friendly_name": "Washing Machine",
			"class":         "cycle_power_device",
			"display_name":  "Washing Machine",
			"location":      "utility",
		},
	})
	serveNamespace(mux, "energy_tariffs", map[string]any{
		"tariffs": map[string]any{
			"electricity": map[string]any{
				"unit_rate":             0.2089,
				"daily_standing_charge": 0.5294,
				"unit":                  "kWh",
				"vat_rate":              0.05,
			},
		},
	})

	f := newTestFetcher(t, mux, &staticTokenSource{token: "test-token"})
	f.Refresh(context.Background())

	devices := f.Devices()
	wm, ok := devices["washingmachine"]
	if !ok {
		t.Fatal("washingmachine missing after refresh")
	}
	if wm.Class != "cycle_power_device" {
		t.Errorf("class: got %q, want cycle_power_device", wm.Class)
	}
	if wm.Scheme != "zigbee" {
		t.Errorf("scheme: got %q, want zigbee (normalised)", wm.Scheme)
	}
	if wm.Primary != "0xaabbccddeeff0011" {
		t.Errorf("primary: got %q, want 0xaabbccddeeff0011 (normalised)", wm.Primary)
	}

	tariff, ok := f.Tariffs().Electricity()
	if !ok {
		t.Fatal("electricity tariff missing after refresh")
	}
	if tariff.UnitRate != 0.2089 {
		t.Errorf("unit_rate: got %v, want 0.2089", tariff.UnitRate)
	}
	if tariff.VATRate != 0.05 {
		t.Errorf("vat_rate: got %v, want 0.05", tariff.VATRate)
	}

	st := f.Statuses()
	if !st["statehouse_devices"].OK {
		t.Error("statehouse_devices status not OK")
	}
	if !st["energy_tariffs"].OK {
		t.Error("energy_tariffs status not OK")
	}
	if st["statehouse_devices"].FetchedAt.IsZero() {
		t.Error("statehouse_devices fetched_at is zero")
	}
}

func TestFetcher_DevicesReturnsCopy(t *testing.T) {
	mux := http.NewServeMux()
	serveNamespace(mux, "statehouse_devices", map[string]any{
		"kettle": map[string]any{"class": "short_burst_power_device"},
	})
	serveNamespace(mux, "energy_tariffs", map[string]any{"tariffs": map[string]any{}})

	f := newTestFetcher(t, mux, &staticTokenSource{token: "test-token"})
	f.Refresh(context.Background())

	got := f.Devices()
	got["kettle"] = DeviceConfig{Class: "mutated"}
	got["injected"] = DeviceConfig{}

	again := f.Devices()
	if again["kettle"].Class != "short_burst_power_device" {
		t.Error("mutating returned map leaked into the held snapshot")
	}
	if _, ok := again["injected"]; ok {
		t.Error("injecting into returned map leaked into the held snapshot")
	}
}

func TestFetcher_401InvalidatesAndKeepsSnapshot(t *testing.T) {
	// Phase 1: a healthy server populates the snapshot.
	good := http.NewServeMux()
	serveNamespace(good, "statehouse_devices", map[string]any{
		"fridge": map[string]any{"class": "continuous_power_device", "display_name": "Fridge"},
	})
	serveNamespace(good, "energy_tariffs", map[string]any{
		"tariffs": map[string]any{
			"electricity": map[string]any{"unit_rate": 0.30, "vat_rate": 0.05},
		},
	})
	goodSrv := httptest.NewServer(good)
	defer goodSrv.Close()

	src := &trackingTokenSource{token: "stale-token"} // not "test-token" -> 401 from serveNamespace
	f := &Fetcher{BaseURL: goodSrv.URL, Tokens: &staticTokenSource{token: "test-token"}, HTTPClient: goodSrv.Client()}
	f.Refresh(context.Background())
	if _, ok := f.Devices()["fridge"]; !ok {
		t.Fatal("precondition: fridge should be present after first refresh")
	}

	// Phase 2: point the fetcher at a server that always 401s.
	bad := http.NewServeMux()
	for _, ns := range []string{"statehouse_devices", "energy_tariffs"} {
		bad.HandleFunc("/api/v1/config/"+ns, func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
		})
	}
	badSrv := httptest.NewServer(bad)
	defer badSrv.Close()
	f.BaseURL = badSrv.URL
	f.HTTPClient = badSrv.Client()
	f.Tokens = src

	f.Refresh(context.Background())

	if !src.invalidated {
		t.Error("expected Invalidate() after 401")
	}
	// Fail-open: prior snapshot is retained.
	if _, ok := f.Devices()["fridge"]; !ok {
		t.Error("fridge snapshot was wiped after a 401 (should fail-open)")
	}
	if _, ok := f.Tariffs().Electricity(); !ok {
		t.Error("tariff snapshot was wiped after a 401 (should fail-open)")
	}
	if f.Statuses()["statehouse_devices"].OK {
		t.Error("status should record the 401 failure")
	}
}

func TestFetcher_OneNamespaceFailureKeepsOther(t *testing.T) {
	mux := http.NewServeMux()
	// devices is healthy; tariffs returns 503.
	serveNamespace(mux, "statehouse_devices", map[string]any{
		"oven": map[string]any{"class": "cycle_power_device", "display_name": "Oven"},
	})
	mux.HandleFunc("/api/v1/config/energy_tariffs", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	})

	f := newTestFetcher(t, mux, &staticTokenSource{token: "test-token"})
	f.Refresh(context.Background())

	if _, ok := f.Devices()["oven"]; !ok {
		t.Error("devices snapshot should be populated despite tariffs failure")
	}
	st := f.Statuses()
	if !st["statehouse_devices"].OK {
		t.Error("statehouse_devices should be OK")
	}
	if st["energy_tariffs"].OK {
		t.Error("energy_tariffs should be failed")
	}
	if st["energy_tariffs"].Error == "" {
		t.Error("energy_tariffs status should carry an error message")
	}
}

func TestFetcher_TokenFailureKeepsSnapshot(t *testing.T) {
	mux := http.NewServeMux()
	serveNamespace(mux, "statehouse_devices", map[string]any{
		"lamp": map[string]any{"class": "continuous_power_device"},
	})
	serveNamespace(mux, "energy_tariffs", map[string]any{"tariffs": map[string]any{}})
	f := newTestFetcher(t, mux, &staticTokenSource{token: "test-token"})
	f.Refresh(context.Background())
	if _, ok := f.Devices()["lamp"]; !ok {
		t.Fatal("precondition: lamp present after first refresh")
	}

	// Swap in a token source that errors.
	f.Tokens = &errTokenSource{}
	f.Refresh(context.Background())

	if _, ok := f.Devices()["lamp"]; !ok {
		t.Error("snapshot wiped after token failure (should fail-open)")
	}
	if f.Statuses()["statehouse_devices"].OK {
		t.Error("status should record token failure")
	}
}

func TestFetcher_EmptyBaseURLNoOp(t *testing.T) {
	f := &Fetcher{Tokens: &errTokenSource{}}
	f.Refresh(context.Background()) // must not panic, must not call Token
	if len(f.Devices()) != 0 {
		t.Error("expected empty devices")
	}
	if len(f.Statuses()) != 0 {
		t.Error("expected no statuses recorded for empty base url")
	}
}

type errTokenSource struct{}

func (errTokenSource) Token(context.Context) (string, error) {
	return "", context.DeadlineExceeded
}
func (errTokenSource) Invalidate() {}
