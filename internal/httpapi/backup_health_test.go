package httpapi

import (
	"net/http"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Backup health.
//
// A backup you are not sure happened is not a backup. The archive is the one thing
// this service writes and the only thing in it that is not rebuildable from Influx,
// so the difference between "backed up" and "configured to back up" has to be
// visible without going to look in a bucket.
//
// The rule mirrors the price archive's: a backup problem DEGRADES rather than
// failing the service. Nothing about serving costs depends on last night's upload,
// so a 503 would be a lie — but a silent "ok" while backups have never once
// succeeded is the failure this reporting exists to catch.
// ---------------------------------------------------------------------------

// fakeBackups reports a fixed BackupHealth.
type fakeBackups struct{ h *BackupHealth }

func (f fakeBackups) BackupHealth() *BackupHealth { return f.h }

// bhNow is the instant dataSetup's injected clock is fixed to. Every timestamp in
// these tests is relative to IT, not to time.Now(): the verdict is computed against
// the server's clock, so a test using wall-clock time puts "four days ago" in the
// clock's future and silently asserts nothing.
var bhNow = time.Date(2026, 6, 11, 13, 0, 0, 0, time.UTC)

// bhSetup is dataSetup with a backup provider installed.
func bhSetup(t *testing.T, h *BackupHealth) *Server {
	t.Helper()
	s, fake := dataSetup(t)
	fake.PingOK = true
	s.Backups = fakeBackups{h}
	if got := s.clock().Now(); !got.Equal(bhNow) {
		t.Fatalf("dataSetup clock is %s, but these tests are written against %s", got, bhNow)
	}
	return s
}

// No provider at all: the block is OMITTED, not rendered empty. A zeroed block
// would read as "backups are broken" on every development box and on every
// flat-rate deployment that has no archive to protect.
func TestHealthOmitsBackupsWhenNotConfigured(t *testing.T) {
	s, fake := dataSetup(t)
	fake.PingOK = true
	m := decode(t, mustGET(t, s, "/healthz"))
	if _, present := m["backup"]; present {
		t.Errorf("an unconfigured deployment reported a backup block: %v", m["backup"])
	}
	if m["status"] != "ok" {
		t.Errorf("status = %v, want ok — not configuring backups is a choice, not a fault", m["status"])
	}
}

// A healthy backup reports when it last succeeded and where it went.
func TestHealthReportsASuccessfulBackup(t *testing.T) {
	now := bhNow
	s := bhSetup(t, &BackupHealth{
		Bucket:      "countinghouse-sqlite",
		Env:         "production",
		Schedule:    "daily",
		Hour:        3,
		LastAttempt: now.Add(-2 * time.Hour),
		LastSuccess: now.Add(-2 * time.Hour),
		LastKey:     "production/backups/countinghouse/2026/09/12/countinghouse-x.sqlite3",
		Successes:   9,
	})
	m := decode(t, mustGET(t, s, "/healthz"))
	b, ok := m["backup"].(map[string]any)
	if !ok {
		t.Fatalf("no backup block: %v", m)
	}
	// The operational signal survives: when it last ran, on what schedule, and
	// whether it worked.
	if b["schedule"] != "daily" {
		t.Errorf("schedule = %v, want daily", b["schedule"])
	}
	if b["last_success"] == nil {
		t.Error("a successful backup must say when it succeeded")
	}
	if m["status"] != "ok" {
		t.Errorf("status = %v, want ok", m["status"])
	}
	// But the infrastructure identifiers do not. This endpoint is unauthenticated,
	// and the key naming where our backups live is of no use to a monitor and of
	// obvious use to anyone else. Both are still on /metrics, behind auth.
	for _, k := range []string{"bucket", "last_key", "env"} {
		if v, present := b[k]; present && v != "" {
			t.Errorf("/healthz still discloses %s = %v", k, v)
		}
	}
}

// Configured but never run. This is the state a typo'd credential or a missing
// bucket leaves behind, and it is the single most important one to surface: the
// operator believes the archive is protected and nothing has ever been uploaded.
func TestHealthDegradesWhenBackupsHaveNeverSucceeded(t *testing.T) {
	s := bhSetup(t, &BackupHealth{
		Bucket:    "countinghouse-sqlite",
		Env:       "production",
		LastError: "upload backup: AccessDenied",
		Failures:  3,
	})
	m := decode(t, mustGET(t, s, "/healthz"))
	if m["status"] != "degraded" {
		t.Errorf("status = %v, want degraded — backups have never succeeded", m["status"])
	}
	b := m["backup"].(map[string]any)
	// The CLASS is reported, not the text: /healthz is unauthenticated, and the
	// message came from an upstream we do not control. The full string is on
	// /metrics, behind auth.
	if b["last_error_class"] == nil {
		t.Error("the failure must be reported, not just the verdict")
	}
	if b["last_error"] != nil {
		t.Errorf("/healthz still carries the raw error: %v", b["last_error"])
	}
}

// Succeeding once then failing is still degraded: the archive has grown since, and
// what is in the bucket no longer covers what would be lost.
func TestHealthDegradesWhenBackupsStartFailing(t *testing.T) {
	now := bhNow
	s := bhSetup(t, &BackupHealth{
		Bucket:      "countinghouse-sqlite",
		Env:         "production",
		LastAttempt: now.Add(-1 * time.Hour),
		LastSuccess: now.Add(-72 * time.Hour),
		LastError:   "upload backup: context deadline exceeded",
		Successes:   1,
		Failures:    2,
	})
	if got := decode(t, mustGET(t, s, "/healthz"))["status"]; got != "degraded" {
		t.Errorf("status = %v, want degraded", got)
	}
}

// A backup that has not run in far too long, with no error to show for it. The
// scheduler being wedged produces exactly this — no failure, just silence — and a
// verdict keyed only on LastError would call it healthy.
func TestHealthDegradesWhenBackupsAreStale(t *testing.T) {
	now := bhNow
	s := bhSetup(t, &BackupHealth{
		Bucket:      "countinghouse-sqlite",
		Env:         "production",
		Schedule:    "daily",
		LastAttempt: now.Add(-100 * time.Hour),
		LastSuccess: now.Add(-100 * time.Hour),
		Successes:   1,
	})
	if got := decode(t, mustGET(t, s, "/healthz"))["status"]; got != "degraded" {
		t.Errorf("status = %v, want degraded — the last backup is four days old", got)
	}
}

// Freshly started, nothing scheduled yet. Must NOT degrade: the service has been up
// for a minute and the first scheduled run is hours away, so complaining would make
// every restart look like a fault.
func TestHealthDoesNotDegradeBeforeTheFirstScheduledRun(t *testing.T) {
	s := bhSetup(t, &BackupHealth{
		Bucket:   "countinghouse-sqlite",
		Env:      "production",
		Schedule: "daily",
		Hour:     3,
		// No attempt, no success, no error: nothing has been asked of it yet.
	})
	m := decode(t, mustGET(t, s, "/healthz"))
	if m["status"] != "ok" {
		t.Errorf("status = %v, want ok — a just-started service has not missed a backup yet",
			m["status"])
	}
	if _, present := m["backup"]; !present {
		t.Error("the block must still appear: configured-but-not-yet-run is worth seeing")
	}
}

// Schedule "off" means on-demand only, so staleness is not a fault — the operator
// said not to run them on a timer.
func TestHealthDoesNotDegradeWhenTheScheduleIsOff(t *testing.T) {
	now := bhNow
	s := bhSetup(t, &BackupHealth{
		Bucket:      "countinghouse-sqlite",
		Env:         "production",
		Schedule:    "off",
		LastAttempt: now.Add(-500 * time.Hour),
		LastSuccess: now.Add(-500 * time.Hour),
		Successes:   1,
	})
	if got := decode(t, mustGET(t, s, "/healthz"))["status"]; got != "ok" {
		t.Errorf("status = %v, want ok — schedule is off, so an old backup is expected", got)
	}
}

// The secret must never reach a response. /healthz is the one endpoint built to be
// polled by anything, and the R2 credential would be the most valuable thing in the
// process to leak.
func TestHealthNeverReportsCredentials(t *testing.T) {
	s := bhSetup(t, &BackupHealth{
		Bucket:    "countinghouse-sqlite",
		Env:       "production",
		LastError: "upload backup: AccessDenied",
	})
	body := mustGET(t, s, "/healthz").Body.String()
	for _, forbidden := range []string{"secret", "access_key", "account_id", "credential"} {
		if containsFold(body, forbidden) {
			t.Errorf("/healthz body mentions %q: %s", forbidden, body)
		}
	}
	if w := doGET(t, s, "/metrics"); w.Code == http.StatusOK {
		mb := w.Body.String()
		for _, forbidden := range []string{"secret", "access_key", "account_id"} {
			if containsFold(mb, forbidden) {
				t.Errorf("/metrics body mentions %q", forbidden)
			}
		}
	}
}

// containsFold is a case-insensitive substring test.
func containsFold(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) &&
		indexFold(haystack, needle) >= 0
}

func indexFold(s, sub string) int {
	lower := func(b byte) byte {
		if b >= 'A' && b <= 'Z' {
			return b + 32
		}
		return b
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		ok := true
		for j := 0; j < len(sub); j++ {
			if lower(s[i+j]) != lower(sub[j]) {
				ok = false
				break
			}
		}
		if ok {
			return i
		}
	}
	return -1
}

// ---------------------------------------------------------------------------

// backupVerdict directly, including a state the HTTP tests reach only indirectly.
// It is pure, so the table is cheap and says what each branch is for.
func TestBackupVerdict(t *testing.T) {
	now := bhNow
	ago := func(d time.Duration) time.Time { return now.Add(-d) }

	tests := []struct {
		name         string
		h            *BackupHealth
		wantDegraded bool
		wantReason   string
	}{
		{
			// No provider: backups are off by choice, which is not a fault.
			name: "nil is not a fault",
		},
		{
			name: "configured, nothing attempted yet",
			h:    &BackupHealth{Schedule: "daily"},
		},
		{
			name:         "an error since the last success",
			h:            &BackupHealth{LastSuccess: ago(time.Hour), LastError: "AccessDenied"},
			wantDegraded: true,
			wantReason:   "failing",
		},
		{
			// Defensive, and deliberately so: backupStatus always records an error
			// alongside a failure today, so this state is unreachable through it. The
			// branch guards the shape where an attempt is timestamped separately from
			// its outcome — a plausible future change that would otherwise turn
			// "tried and never worked" into a silent ok.
			name:         "attempted, never succeeded, no error recorded",
			h:            &BackupHealth{LastAttempt: ago(time.Hour), Schedule: "daily"},
			wantDegraded: true,
			wantReason:   "never been backed up",
		},
		{
			name: "succeeded within the staleness window",
			h:    &BackupHealth{LastSuccess: ago(backupStaleAfter - time.Hour), Schedule: "daily"},
		},
		{
			// Exactly at the threshold is NOT stale: the comparison is strict, so a
			// backup taken precisely 48h ago does not flap in and out on clock jitter.
			name: "succeeded exactly at the threshold",
			h:    &BackupHealth{LastSuccess: ago(backupStaleAfter), Schedule: "daily"},
		},
		{
			name:         "one second past the threshold",
			h:            &BackupHealth{LastSuccess: ago(backupStaleAfter + time.Second), Schedule: "daily"},
			wantDegraded: true,
			wantReason:   "stale",
		},
		{
			// "off" means on-demand only, so an ancient backup is what was asked for.
			name: "stale but the schedule is off",
			h:    &BackupHealth{LastSuccess: ago(500 * time.Hour), Schedule: "off"},
		},
		{
			// A future LastSuccess (a clock step, or a restored backup from a host
			// whose clock ran ahead) must not read as stale. Sub goes negative, which
			// is not greater than the window.
			name: "a success in the future is not stale",
			h:    &BackupHealth{LastSuccess: now.Add(time.Hour), Schedule: "daily"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			degraded, reason := backupVerdict(tc.h, now)
			if degraded != tc.wantDegraded {
				t.Errorf("degraded = %v, want %v (reason %q)", degraded, tc.wantDegraded, reason)
			}
			if tc.wantReason != "" && !containsFold(reason, tc.wantReason) {
				t.Errorf("reason = %q, want it to mention %q", reason, tc.wantReason)
			}
			if !degraded && reason != "" {
				t.Errorf("healthy but gave a reason: %q", reason)
			}
			if degraded && reason == "" {
				t.Error("degraded with no reason; the verdict must say why")
			}
		})
	}
}

// /healthz must say WHY it is degraded. Both verdict functions computed a reason and both
// callers discarded it, so an operator saw "degraded" and had to work out which of four
// conditions caused it — with the answer sitting in a thrown-away return value.
func TestHealthReportsWhyItIsDegraded(t *testing.T) {
	now := bhNow
	s := bhSetup(t, &BackupHealth{
		Bucket: "countinghouse-sqlite", Env: "production",
		LastError: "AccessDenied", Failures: 2,
	})
	s.Prices = fakePrices{health: []PriceHealth{{
		TariffCode: "E-1R-AGILE-24-10-01-A",
		KnownTo:    now.Add(12 * time.Hour),
		LastError:  "429 from the supplier",
	}}}

	m := decode(t, mustGET(t, s, "/healthz"))
	if m["status"] != "degraded" {
		t.Fatalf("status = %v, want degraded", m["status"])
	}
	reasons, ok := m["reasons"].([]any)
	if !ok || len(reasons) == 0 {
		t.Fatalf("no reasons reported: %v", m)
	}
	// Both conditions, not just the last one to win the assignment.
	joined := ""
	for _, r := range reasons {
		joined += r.(string) + " | "
	}
	if !containsFold(joined, "price fetch failing") {
		t.Errorf("the price condition is missing: %q", joined)
	}
	if !containsFold(joined, "backup") {
		t.Errorf("the backup condition is missing: %q", joined)
	}
}

// A healthy service reports no reasons at all, rather than an empty array.
func TestHealthOmitsReasonsWhenOK(t *testing.T) {
	s, fake := dataSetup(t)
	fake.PingOK = true
	m := decode(t, mustGET(t, s, "/healthz"))
	if m["status"] != "ok" {
		t.Fatalf("status = %v, want ok", m["status"])
	}
	if _, present := m["reasons"]; present {
		t.Errorf("a healthy service reported reasons: %v", m["reasons"])
	}
}
