package httpapi

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/sweeney/countinghouse/internal/config"
)

// ---------------------------------------------------------------------------
// Issue #36 finding #6 and N1.
//
// /tariffs carried `source` but no timestamp of any kind, so a consumer reading
// it had no way to know it held a stale snapshot without a second call to
// /healthz and a cross-reference. Given "boot needs truth, running keeps the last
// truth", that timestamp is the only signal a data consumer has.
//
// And the window caps were stated in prose only: /series in BUCKETS, /prices in
// DAYS, with nothing in the API to reconcile them.
// ---------------------------------------------------------------------------

// fakeStatuses is a ConfigStatus over a fixed map.
type fakeStatuses map[string]config.NamespaceStatus

func (f fakeStatuses) Statuses() map[string]config.NamespaceStatus {
	return map[string]config.NamespaceStatus(f)
}

func TestTariffs_ReportsFreshness(t *testing.T) {
	s := floorSeriesSetup(t)
	at := time.Date(2026, 9, 21, 6, 15, 2, 0, time.UTC)
	s.RemoteConfig = fakeStatuses{"energy_tariffs": {OK: true, FetchedAt: at}}

	m := decode(t, doGET(t, s, "/tariffs"))
	if m["fetched_at"] == nil {
		t.Fatalf("/tariffs carries no fetch timestamp: %v", m)
	}
	if got, want := m["fetched_at"].(string), at.Format(time.RFC3339); got[:19] != want[:19] {
		t.Errorf("fetched_at = %q, want %q", got, want)
	}
	if m["stale"] != false {
		t.Errorf("stale = %v, want false after a successful fetch", m["stale"])
	}
}

// The fail-open design means a failed refresh keeps serving the last good
// snapshot while /healthz merely degrades. stale says so at the point of use.
func TestTariffs_ReportsStaleAfterAFailedFetch(t *testing.T) {
	s := floorSeriesSetup(t)
	s.RemoteConfig = fakeStatuses{"energy_tariffs": {
		OK: false, FetchedAt: time.Date(2026, 9, 18, 0, 0, 0, 0, time.UTC), Error: "boom",
	}}

	m := decode(t, doGET(t, s, "/tariffs"))
	if m["stale"] != true {
		t.Errorf("stale = %v, want true: the last fetch failed", m["stale"])
	}
	// The error text itself stays on /healthz; /tariffs says only that the data
	// is not fresh.
	if _, leaked := m["error"]; leaked {
		t.Error("/tariffs should not carry the fetch error text")
	}
}

// Nothing to report is reported as nothing, not as a zero time.
func TestTariffs_OmitsFreshnessWithNoFetcher(t *testing.T) {
	s := floorSeriesSetup(t)
	s.RemoteConfig = nil
	m := decode(t, doGET(t, s, "/tariffs"))
	if _, ok := m["fetched_at"]; ok {
		t.Error("no fetcher wired, but a fetched_at was invented")
	}
	if _, ok := m["stale"]; ok {
		t.Error("no fetcher wired, but a stale flag was invented")
	}
}

// The same two fields on the floorplan catalogs, from their own namespace.
func TestFloorsAndRooms_ReportFreshness(t *testing.T) {
	s := floorSeriesSetup(t)
	s.FloorplanNamespace = "floorplan_home"
	s.RemoteConfig = fakeStatuses{"floorplan_home": {
		OK: false, FetchedAt: time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC),
	}}
	for _, path := range []string{"/floors", "/rooms"} {
		m := decode(t, doGET(t, s, path))
		if m["fetched_at"] == nil {
			t.Errorf("%s carries no fetch timestamp: %v", path, m)
		}
		if m["stale"] != true {
			t.Errorf("%s stale = %v, want true", path, m["stale"])
		}
	}
}

// available_now turns the expired-baseline error from a silence into a field.
func TestTariffs_MarksWhichAgreementIsAvailableNow(t *testing.T) {
	s := floorSeriesSetup(t)
	m := decode(t, doGET(t, s, "/tariffs"))

	agreements, ok := m["agreements"].(map[string]any)
	if !ok {
		t.Fatalf("no agreements: %v", m)
	}
	elec, ok := agreements["electricity"].([]any)
	if !ok || len(elec) == 0 {
		t.Fatalf("no electricity agreements: %v", agreements)
	}
	for i, raw := range elec {
		row := raw.(map[string]any)
		if _, present := row["available_now"]; !present {
			t.Errorf("agreement %d has no available_now: %v", i, row)
		}
		// The descriptive fields still come through: the wrapper embeds rather
		// than copying, so nothing is lost.
		if row["name"] == nil {
			t.Errorf("agreement %d lost its name: %v", i, row)
		}
	}
}

// An agreement that has ended is not available, one covering now is, and the
// clock is the injected one rather than time.Now.
func TestTariffs_AvailableNowUsesTheInjectedClock(t *testing.T) {
	s, _ := dataSetup(t)
	past := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	ended := time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC)
	current := time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC)
	ag := config.EnergyAgreements{
		Agreements: map[string][]config.Agreement{"electricity": {
			{From: &past, To: &ended, Name: "Expired", Type: config.TariffTypeFixed, VATRate: 0.05, UnitRate: 0.20},
			{From: &current, Name: "Current", Type: config.TariffTypeFixed, VATRate: 0.05, UnitRate: 0.21},
		}},
	}
	s.Config = fakeConfig{devices: testDevices(), tariffs: testTariffs(), agreements: &ag}

	m := decode(t, doGET(t, s, "/tariffs"))
	elec := m["agreements"].(map[string]any)["electricity"].([]any)
	if len(elec) != 2 {
		t.Fatalf("want 2 agreements, got %d", len(elec))
	}
	if elec[0].(map[string]any)["available_now"] != false {
		t.Error("an agreement that ended in 2021 is not available now")
	}
	if elec[1].(map[string]any)["available_now"] != true {
		t.Error("the open-ended current agreement should be available now")
	}
}

// ---------------------------------------------------------------------------
// N1: the caps, as data.
// ---------------------------------------------------------------------------

func TestSeries_BucketCapRefusalCarriesTheNumbers(t *testing.T) {
	s := floorSeriesSetup(t)
	w := doGET(t, s, "/series?window=custom&from=2026-01-01T00:00:00Z&to=2026-03-01T00:00:00Z&interval=30m")
	if w.Code != 400 {
		t.Fatalf("want 400, got %d: %s", w.Code, w.Body.String())
	}
	var body struct {
		Error  string `json:"error"`
		Limits struct {
			MaxBuckets        int    `json:"max_buckets"`
			Buckets           int    `json:"buckets"`
			Interval          string `json:"interval"`
			SuggestedInterval string `json:"suggested_interval"`
			MaxWindowSeconds  int64  `json:"max_window_seconds"`
		} `json:"limits"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// The prose is kept — it was never the problem.
	if body.Error == "" {
		t.Error("the explanatory message was dropped")
	}
	if body.Limits.MaxBuckets != 1000 {
		t.Errorf("max_buckets = %d, want 1000", body.Limits.MaxBuckets)
	}
	if body.Limits.Buckets <= 1000 {
		t.Errorf("buckets = %d, should exceed the cap", body.Limits.Buckets)
	}
	if body.Limits.Interval != "30m" {
		t.Errorf("interval = %q, want 30m", body.Limits.Interval)
	}
	if body.Limits.SuggestedInterval == "" {
		t.Error("no suggested_interval")
	}
	// The key field: the cap in the same unit /prices states its cap in, so one
	// chunking routine can serve both. 1000 × 30m.
	if want := int64(1000 * 30 * 60); body.Limits.MaxWindowSeconds != want {
		t.Errorf("max_window_seconds = %d, want %d", body.Limits.MaxWindowSeconds, want)
	}
}

// The price routes state their cap in DAYS. Same limits block, same
// max_window_seconds key, so one chunking routine reads both.
func TestPrices_DayCapRefusalCarriesTheNumbers(t *testing.T) {
	s := pxFlatConfig(t)
	w := doGET(t, s, "/prices?window=custom&from=2026-01-01T00:00:00Z&to=2026-06-01T00:00:00Z")
	if w.Code != 400 {
		t.Fatalf("want 400, got %d: %s", w.Code, w.Body.String())
	}
	var body struct {
		Error  string `json:"error"`
		Limits struct {
			MaxDays          int     `json:"max_days"`
			Days             float64 `json:"days"`
			MaxWindowSeconds int64   `json:"max_window_seconds"`
		} `json:"limits"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Error == "" {
		t.Error("the explanatory message was dropped")
	}
	if body.Limits.MaxDays != 31 {
		t.Errorf("max_days = %d, want 31", body.Limits.MaxDays)
	}
	if want := int64(31 * 86400); body.Limits.MaxWindowSeconds != want {
		t.Errorf("max_window_seconds = %d, want %d", body.Limits.MaxWindowSeconds, want)
	}
	if body.Limits.Days <= 31 {
		t.Errorf("days = %v, should exceed the cap", body.Limits.Days)
	}
}
