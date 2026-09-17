package httpapi

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// /healthz is UNAUTHENTICATED (publicRoutes). It must carry a verdict, not
// evidence.
//
// It had grown to reflect up to 2 KB of verbatim upstream response body via
// APIError.Error(), plus local archive paths, the R2 bucket and the full object
// key. No credential leak today — but arbitrary third-party bytes and
// infrastructure identifiers on a public endpoint, resting on an upstream
// redactor this repo cannot enforce or test.
//
// The full strings stay on /metrics, which is behind auth.
// ---------------------------------------------------------------------------

type leakyPrices struct{ h []PriceHealth }

func (l leakyPrices) PriceHealth() []PriceHealth { return l.h }

func TestHealthzDoesNotEchoUpstreamErrorBodies(t *testing.T) {
	s, _ := dataSetup(t)
	s.Prices = leakyPrices{h: []PriceHealth{{
		TariffCode: pxTariff,
		LastError: `octopus: GET https://api.octopus.energy/v1/products/AGILE/...?page=3 ` +
			`returned 500: {"detail":"internal","trace":"SECRET-TRACE-abc123",` +
			`"upstream":"10.1.2.3:5432"}`,
		LastAttempt: time.Now(),
	}}}

	body := doGET(t, s, "/healthz").Body.String()
	for _, leaked := range []string{"SECRET-TRACE-abc123", "10.1.2.3:5432", "api.octopus.energy", "page=3"} {
		if strings.Contains(body, leaked) {
			t.Errorf("/healthz echoes %q from an upstream error body", leaked)
		}
	}
	// It must still SAY something is wrong.
	if !strings.Contains(body, "degraded") && !strings.Contains(body, "unavailable") {
		t.Error("/healthz hid the failure entirely; it should report a class, not nothing")
	}
}

func TestHealthzDoesNotDiscloseArchivePaths(t *testing.T) {
	s, _ := dataSetup(t)
	s.Prices = leakyPrices{h: []PriceHealth{{
		TariffCode:  pxTariff,
		LastError:   "prices: open archive at /var/lib/countinghouse/prices.db: permission denied",
		LastAttempt: time.Now(),
	}}}
	body := doGET(t, s, "/healthz").Body.String()
	if strings.Contains(body, "/var/lib/countinghouse") {
		t.Error("/healthz discloses the local archive path")
	}
}

func TestHealthzDoesNotDiscloseBackupInfrastructure(t *testing.T) {
	s, _ := dataSetup(t)
	s.Backups = staticBackup{h: &BackupHealth{
		Bucket:      "countinghouse-sqlite",
		Env:         "production",
		LastKey:     "production/backups/countinghouse/2026/09/16/countinghouse-2026-09-16T03:00:00Z.sqlite3",
		LastError:   "r2: PutObject 403 AccessDenied for key production/backups/...",
		LastAttempt: time.Now(),
	}}
	body := doGET(t, s, "/healthz").Body.String()
	for _, leaked := range []string{"countinghouse-sqlite", "AccessDenied", "production/backups"} {
		if strings.Contains(body, leaked) {
			t.Errorf("/healthz discloses %q", leaked)
		}
	}
}

type staticBackup struct{ h *BackupHealth }

func (b staticBackup) BackupHealth() *BackupHealth { return b.h }

// The other half of the decision: the detail is not destroyed, only moved behind
// auth. /metrics is in dataRoutes, so an operator still gets the whole string.
func TestMetricsKeepsTheFullErrorDetail(t *testing.T) {
	s, _ := dataSetup(t)
	const detail = "octopus: GET https://api.octopus.energy/v1/... returned 500: SECRET-TRACE-abc123"
	s.Prices = leakyPrices{h: []PriceHealth{{
		TariffCode: pxTariff, LastError: detail, LastAttempt: time.Now(),
	}}}

	if body := doGET(t, s, "/healthz").Body.String(); strings.Contains(body, "SECRET-TRACE") {
		t.Fatal("precondition: /healthz should already be redacted")
	}
	body := doGET(t, s, "/metrics").Body.String()
	if !strings.Contains(body, "SECRET-TRACE-abc123") {
		t.Error("/metrics lost the detail; it is behind auth and is where an operator looks")
	}
}

// Redaction must not hide that something IS wrong — the class still reaches the
// top-level status and reasons, which is what a monitor alerts on.
func TestHealthzStillDegradesOnACollectorFailure(t *testing.T) {
	s, _ := dataSetup(t)
	s.Prices = leakyPrices{h: []PriceHealth{{
		TariffCode: pxTariff,
		// Both fields, as a real provider supplies them: the collector derives the
		// class from the typed error and keeps the text for /metrics.
		LastError:      "collector: horizon probe: octopus: 429 Too Many Requests (429) for https://api.octopus.energy/v1/x: throttled",
		LastErrorClass: "upstream rate limited",
		LastAttempt:    time.Now(),
	}}}
	m := decode(t, doGET(t, s, "/healthz"))
	if m["status"] == "ok" {
		t.Error("status is ok despite a failing collector")
	}
	body := doGET(t, s, "/healthz").Body.String()
	if !strings.Contains(body, "rate limited") {
		t.Errorf("the class should survive redaction so the reason is actionable: %s", body)
	}
}

// ---------------------------------------------------------------------------
// The split is only real if the spec says so. /healthz and /metrics referenced ONE
// pair of schemas, so the published contract promised anonymous callers a
// `last_error`, a `bucket` and an object key that redaction had already removed —
// a generated client would offer a field that is never set, and an operator would
// write the alert that never fires.
//
// So the redacted schemas are checked against what the handler actually emits, in
// BOTH directions: a field served but undocumented is a disclosure nobody reviewed,
// and a field documented but never served is a promise. Populating every source
// field is what makes the second direction meaningful.
// ---------------------------------------------------------------------------

// specSchemaProps returns the property names of one component schema.
func specSchemaProps(t *testing.T, s *Server, name string) map[string]bool {
	t.Helper()
	var doc struct {
		Components struct {
			Schemas map[string]struct {
				Properties map[string]json.RawMessage `json:"properties"`
			} `json:"schemas"`
		} `json:"components"`
	}
	body := doGET(t, s, "/openapi.json").Body.Bytes()
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("unmarshal spec: %v", err)
	}
	sc, ok := doc.Components.Schemas[name]
	if !ok {
		t.Fatalf("schema %s is not in the spec", name)
	}
	out := make(map[string]bool, len(sc.Properties))
	for p := range sc.Properties {
		out[p] = true
	}
	return out
}

func compareKeys(t *testing.T, what string, served map[string]any, documented map[string]bool) {
	t.Helper()
	for k := range served {
		if !documented[k] {
			t.Errorf("%s serves %q, which the redacted schema does not document", what, k)
		}
	}
	for k := range documented {
		if _, ok := served[k]; !ok {
			t.Errorf("%s documents %q but never serves it", what, k)
		}
	}
}

func TestHealthzMatchesTheRedactedSchemas(t *testing.T) {
	s, _ := dataSetup(t)
	now := time.Now().UTC().Truncate(time.Second)
	// Every source field populated, so "documented but not served" is a real finding
	// rather than an artefact of omitempty.
	s.Prices = leakyPrices{h: []PriceHealth{{
		TariffCode: pxTariff, KnownTo: now, CompleteTo: now,
		LastAttempt: now, LastSuccess: now,
		LastError: "octopus: returned 500: SECRET",
		Syncs:     3, Failures: 1, Inserted: 96, Restated: 2, Rejected: 1, Warnings: 4,
	}}}
	s.Backups = staticBackup{h: &BackupHealth{
		Bucket: "countinghouse-sqlite", Env: "production", Schedule: "daily", Hour: 3,
		LastAttempt: now, LastSuccess: now,
		LastKey:   "production/backups/countinghouse/2026/09/16/countinghouse-x.sqlite3",
		LastError: "r2: PutObject 403 AccessDenied",
		Successes: 9, Failures: 1, NextRun: now.Add(24 * time.Hour),
	}}

	m := decode(t, doGET(t, s, "/healthz"))
	prices, ok := m["prices"].([]any)
	if !ok || len(prices) == 0 {
		t.Fatalf("no prices block on /healthz: %v", m["prices"])
	}
	compareKeys(t, "/healthz prices[]", prices[0].(map[string]any),
		specSchemaProps(t, s, "PriceHealthRedacted"))
	compareKeys(t, "/healthz backup", m["backup"].(map[string]any),
		specSchemaProps(t, s, "BackupHealthRedacted"))
}
