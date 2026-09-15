package main

import (
	"testing"
	"time"

	"github.com/sweeney/countinghouse/internal/config"
	"github.com/sweeney/countinghouse/internal/testutil"
)

// ---------------------------------------------------------------------------
// What is left of countinghouse's backup code, and therefore what is left to test.
//
// This file used to test a local snapshot, a local schedule, local status bookkeeping and
// a local credential redactor — roughly 230 lines of implementation that existed only
// because common/backup.Manager could not be asked what it had done and could not be
// tested against a clock. All four are now upstream (identity#45 / PR #46), and each
// property those tests pinned is covered there:
//
//	WAL-consistent snapshot   TestCopyDB_IncludesCommittedWALData
//	status semantics          TestManager_Status_FailureDoesNotMoveLastSuccess, and others
//	hour 0 is midnight        TestNewManager_ScheduleHourZeroIsMidnight
//	error redaction           TestManager_Status_LastErrorIsRedacted, TestRedactSecrets_*
//	key layout                TestManager_RunNow_KeyFormat_UsesConfiguredService
//
// Deleting a test is only safe when the property survives somewhere, so each was checked
// against the module cache before its local version went. What remains ours is the
// ADAPTER: config in, /healthz block out.
// ---------------------------------------------------------------------------

// configuredBackup is a complete backup block. The credentials are never exercised: the
// R2 client is built locally and the first real exchange is the first upload.
func configuredBackup() config.Config {
	cfg := config.Default()
	cfg.Prices.DBPath = "/tmp/countinghouse-test-prices.db"
	cfg.Prices.Backup = config.BackupConfig{
		Env: config.EnvProduction, Bucket: "countinghouse-sqlite",
		AccountID: "acct", AccessKeyID: "akid", SecretAccessKey: "secret",
		Hour: 3,
	}
	return cfg
}

// No backup block returns a NIL provider, which is what omits the /healthz block
// entirely. Returning an empty one would render a zeroed block, and a zeroed block reads
// as a broken backup on every development box rather than as no backup.
func TestStartBackupsUnconfiguredReturnsNil(t *testing.T) {
	cfg := config.Default()
	cfg.Prices.DBPath = "/tmp/prices.db"
	if p := startBackups(t.Context(), cfg, testutil.RealClock{}, quietLogger()); p != nil {
		t.Errorf("an unconfigured deployment got a provider: %+v", p)
	}
}

// Configured, it reports the destination that was ASKED for alongside whatever has
// HAPPENED — which is why bucket/env/schedule/hour come from config rather than from
// Status. /healthz wants both, and Status only knows the second.
func TestStartBackupsReportsItsDestinationAndSchedule(t *testing.T) {
	clock := testutil.NewFakeClock(time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC))
	p := startBackups(t.Context(), configuredBackup(), clock, quietLogger())
	if p == nil {
		t.Fatal("a configured deployment got no provider")
	}
	h := p.BackupHealth()

	if h.Bucket != "countinghouse-sqlite" || h.Env != config.EnvProduction {
		t.Errorf("destination = %s/%s", h.Env, h.Bucket)
	}
	// An empty schedule must be reported as what will actually happen. The library's
	// empty-means-daily rule is resolved once, here, so /healthz never shows a blank.
	if h.Schedule != "daily" {
		t.Errorf("Schedule = %q, want daily", h.Schedule)
	}
	if h.Hour != 3 {
		t.Errorf("Hour = %d, want 3", h.Hour)
	}
	// Nothing has run yet, so the verdict side must see a clean slate.
	if !h.LastAttempt.IsZero() || !h.LastSuccess.IsZero() || h.Failures != 0 {
		t.Errorf("a just-started provider reports activity: %+v", h)
	}
}

// NextRun comes from the Manager and is driven by the INJECTED clock, which is the whole
// point of the upstream change: the schedule is now observable and testable from here
// rather than only by waiting a day.
func TestStartBackupsNextRunFollowsTheInjectedClock(t *testing.T) {
	// 12:00 UTC with backups due at 03:00 means tomorrow.
	clock := testutil.NewFakeClock(time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC))
	h := startBackups(t.Context(), configuredBackup(), clock, quietLogger()).BackupHealth()

	if h.NextRun.IsZero() {
		t.Fatal("NextRun is zero on a scheduled backup; /healthz cannot say whether one is coming")
	}
	want := time.Date(2026, 9, 16, 3, 0, 0, 0, time.UTC)
	if !h.NextRun.Equal(want) {
		t.Errorf("NextRun = %s, want %s — it must derive from the injected clock, not time.Now",
			h.NextRun, want)
	}
}

// Hour 0 means MIDNIGHT, end to end through our config and the library. This was a real
// v0.3.0 bug (0 was read as unset and became 03:00), it is fixed upstream, and the
// end-to-end assertion is still ours to make because the config layer is ours.
func TestStartBackupsHourZeroIsMidnight(t *testing.T) {
	cfg := configuredBackup()
	cfg.Prices.Backup.Hour = 0
	clock := testutil.NewFakeClock(time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC))
	h := startBackups(t.Context(), cfg, clock, quietLogger()).BackupHealth()

	if h.Hour != 0 {
		t.Errorf("Hour = %d, want 0", h.Hour)
	}
	want := time.Date(2026, 9, 16, 0, 0, 0, 0, time.UTC)
	if !h.NextRun.Equal(want) {
		t.Errorf("NextRun = %s, want midnight (%s)", h.NextRun, want)
	}
}

// Schedule "off" means on-demand only: no NextRun, because nothing is coming on a timer.
// That is different from backups being unconfigured, and /healthz must not confuse them —
// which is why backupVerdict suppresses staleness when the schedule is off.
func TestStartBackupsScheduleOffHasNoNextRun(t *testing.T) {
	cfg := configuredBackup()
	cfg.Prices.Backup.Schedule = "off"
	h := startBackups(t.Context(), cfg, testutil.RealClock{}, quietLogger()).BackupHealth()

	if h.Schedule != "off" {
		t.Errorf("Schedule = %q, want off", h.Schedule)
	}
	if !h.NextRun.IsZero() {
		t.Errorf("NextRun = %s with the schedule off; nothing is coming on a timer", h.NextRun)
	}
}
