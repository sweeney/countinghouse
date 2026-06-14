package httpapi

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sweeney/countinghouse/internal/config"
	"github.com/sweeney/countinghouse/internal/influx"
)

// TestWriteJSON_NonFiniteYields500 locks issue #4: encoding/json cannot marshal
// NaN/±Inf, so if a non-finite value reaches writeJSON it must surface as a
// real 500 with a well-formed JSON error body — NOT a 200 with a
// truncated/empty body that looks successful. writeJSON encodes to a buffer
// first so the status can still be downgraded when Encode fails.
func TestWriteJSON_NonFiniteYields500(t *testing.T) {
	w := httptest.NewRecorder()
	writeJSON(w, http.StatusOK, map[string]any{"kwh": math.NaN()})

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("want 500 on unmarshalable value, got %d (body=%q)", w.Code, w.Body.String())
	}
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("response body is not well-formed JSON: %v (body=%q)", err, w.Body.String())
	}
	if m["error"] == nil {
		t.Errorf("want an error field in the 500 body, got %v", m)
	}
}

// TestWriteJSON_HappyPath proves the buffered path doesn't regress the normal
// case: a marshalable value still yields the requested status, the JSON
// content-type, and a complete body.
func TestWriteJSON_HappyPath(t *testing.T) {
	w := httptest.NewRecorder()
	writeJSON(w, http.StatusOK, map[string]any{"hello": "world"})

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Errorf("content-type = %q", ct)
	}
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m["hello"] != "world" {
		t.Errorf("body = %v", m)
	}
}

// fakeConfigStatus is a ConfigStatus test double.
type fakeConfigStatus struct {
	statuses map[string]config.NamespaceStatus
}

func (f fakeConfigStatus) Statuses() map[string]config.NamespaceStatus { return f.statuses }

// setup returns a Server wired with a FakeQuerier (PingOK=true) and auth
// disabled (IdentityURL=""), for the non-auth tests. Auth tests override
// IdentityURL via authSetup.
func setup(t *testing.T) *Server {
	t.Helper()
	q := &influx.FakeQuerier{PingOK: true}
	return New(":0", q, nil)
}

func TestHandleHealth_OK(t *testing.T) {
	s := setup(t)
	mux := newMux(s)
	r := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Errorf("want application/json; charset=utf-8, got %q", ct)
	}

	var h struct {
		Status          string `json:"status"`
		StartedAt       string `json:"started_at"`
		StartedAgo      int    `json:"started_ago"`
		Goroutines      int    `json:"goroutines"`
		InfluxReachable bool   `json:"influx_reachable"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &h); err != nil {
		t.Fatalf("unmarshal health: %v", err)
	}
	if h.Status != "ok" {
		t.Errorf("want status ok, got %q", h.Status)
	}
	if h.StartedAt == "" {
		t.Error("want non-empty started_at")
	}
	if h.Goroutines <= 0 {
		t.Errorf("want positive goroutines, got %d", h.Goroutines)
	}
	if !h.InfluxReachable {
		t.Error("want influx_reachable true (FakeQuerier PingOK=true)")
	}
}

func TestHandleHealth_InfluxUnreachable(t *testing.T) {
	s := setup(t)
	s.Influx = &influx.FakeQuerier{PingOK: false}
	mux := newMux(s)
	r := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	var h struct {
		InfluxReachable bool `json:"influx_reachable"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &h); err != nil {
		t.Fatalf("unmarshal health: %v", err)
	}
	if h.InfluxReachable {
		t.Error("want influx_reachable false (FakeQuerier PingOK=false)")
	}
}

// TestHandleHealth_StatusDerivation locks issue #2: the top-level status must
// reflect downstream reachability, not be a hard-coded "ok". Influx is the hard
// dependency (unreachable -> "unavailable"); a failing remote-config namespace
// is "degraded" (we still serve last-known-good); all healthy -> "ok". Influx
// takes precedence over a degraded namespace.
func TestHandleHealth_StatusDerivation(t *testing.T) {
	okNS := map[string]config.NamespaceStatus{
		"statehouse_devices": {OK: true},
		"energy_tariffs":     {OK: true},
	}
	degradedNS := map[string]config.NamespaceStatus{
		"statehouse_devices": {OK: true},
		"energy_tariffs":     {OK: false, Error: "boom"},
	}
	cases := []struct {
		name   string
		ping   bool
		ns     map[string]config.NamespaceStatus
		want   string
		wantHC int
	}{
		{"all healthy", true, okNS, "ok", http.StatusOK},
		{"all healthy, no fetcher", true, nil, "ok", http.StatusOK},
		{"degraded namespace", true, degradedNS, "degraded", http.StatusOK},
		{"influx down", false, okNS, "unavailable", http.StatusOK},
		{"influx down beats degraded", false, degradedNS, "unavailable", http.StatusOK},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := setup(t)
			s.Influx = &influx.FakeQuerier{PingOK: c.ping}
			if c.ns != nil {
				s.RemoteConfig = fakeConfigStatus{statuses: c.ns}
			}
			mux := newMux(s)
			r := httptest.NewRequest(http.MethodGet, "/healthz", nil)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, r)

			if w.Code != c.wantHC {
				t.Fatalf("status code = %d, want %d", w.Code, c.wantHC)
			}
			var h struct {
				Status string `json:"status"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &h); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if h.Status != c.want {
				t.Errorf("status = %q, want %q (body=%s)", h.Status, c.want, w.Body.String())
			}
		})
	}
}

func TestHandleHealth_RemoteConfig(t *testing.T) {
	s := setup(t)
	s.RemoteConfig = fakeConfigStatus{statuses: map[string]config.NamespaceStatus{
		"statehouse_devices": {OK: true},
		"energy_tariffs":     {OK: false, Error: "boom"},
	}}
	mux := newMux(s)
	r := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)

	var h struct {
		RemoteConfig map[string]config.NamespaceStatus `json:"remote_config"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &h); err != nil {
		t.Fatalf("unmarshal health: %v", err)
	}
	if !h.RemoteConfig["statehouse_devices"].OK {
		t.Error("want statehouse_devices OK")
	}
	if h.RemoteConfig["energy_tariffs"].OK {
		t.Error("want energy_tariffs not OK")
	}
	if h.RemoteConfig["energy_tariffs"].Error != "boom" {
		t.Errorf("want energy_tariffs error 'boom', got %q", h.RemoteConfig["energy_tariffs"].Error)
	}
}

func TestHandleHealth_RemoteConfigOmittedWhenNil(t *testing.T) {
	s := setup(t) // RemoteConfig nil
	mux := newMux(s)
	r := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if got := w.Body.String(); contains(got, "remote_config") {
		t.Errorf("remote_config should be omitted when RemoteConfig is nil; body=%s", got)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestHandleHealth_Version(t *testing.T) {
	s := setup(t)
	s.Version = "abc123"
	mux := newMux(s)
	r := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)

	var h struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &h); err != nil {
		t.Fatalf("unmarshal health: %v", err)
	}
	if h.Version != "abc123" {
		t.Errorf("want version abc123, got %q", h.Version)
	}
}
