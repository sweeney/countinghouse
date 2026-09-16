package httpapi

import (
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
		// The real shape APIError.Error() renders, not an invented one.
		LastError:   "octopus: Too Many Requests (429) for https://api.octopus.energy/v1/x: throttled",
		LastAttempt: time.Now(),
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
