package main

import (
	"context"
	"log/slog"

	"github.com/sweeney/identity/common/backup"

	"github.com/sweeney/countinghouse/internal/config"
	"github.com/sweeney/countinghouse/internal/httpapi"
	"github.com/sweeney/countinghouse/internal/testutil"
)

// ---------------------------------------------------------------------------
// Offsite backup of the price archive.
//
// The archive is the one thing countinghouse writes, and the only state here that is NOT
// rebuildable from Influx — CLAUDE.md is explicit that it is primary durable state
// "which is exactly why it gets an enforced key, a restatement log and a backup — not a
// retention policy". This is that backup.
//
// Unconfigured is a legitimate, silent state: a development box, or any deployment whose
// agreements are all flat-rate and which therefore has no archive. A PARTIALLY configured
// one is not — config.BackupConfig.Validate refuses it at startup, because a backup that
// is quietly not happening is worse than none.
//
// This file used to be ~230 lines: its own snapshot, its own schedule loop, its own
// status bookkeeping and its own credential redactor, because `common/backup.Manager`
// could not be asked what it had done and could not be tested against a clock. All four
// gaps are now closed upstream (identity#45 / PR #46), so all four local versions are
// gone and the Manager does the work.
//
// What remains is the adapter: config to backup.Config, and backup.Status to the
// /healthz block. Which is what the issue was filed to make possible.
// ---------------------------------------------------------------------------

// backupProvider adapts a *backup.Manager to httpapi.BackupProvider.
//
// The bucket, env, schedule and hour come from config rather than from Status: they are
// what was ASKED for, the Manager reports what HAPPENED, and /healthz wants both.
type backupProvider struct {
	mgr    *backup.Manager
	bucket string
	env    string
	sched  string
	hour   int
}

// BackupHealth implements httpapi.BackupProvider.
//
// A straight projection now. LastError arrives already passed through the library's
// RedactSecrets, so there is nothing to scrub here — and the local redactor that used to
// do it was blunter, truncating at the first marker word where the library's is
// key-aware.
func (b backupProvider) BackupHealth() *httpapi.BackupHealth {
	st := b.mgr.Status()
	return &httpapi.BackupHealth{
		Bucket:      b.bucket,
		Env:         b.env,
		Schedule:    b.sched,
		Hour:        b.hour,
		LastAttempt: st.LastAttempt,
		LastSuccess: st.LastSuccess,
		LastKey:     st.LastKey,
		LastError:   st.LastError,
		Successes:   st.Successes,
		Failures:    st.Failures,
		NextRun:     st.NextRun,
	}
}

// startBackups builds and starts the archive's backup, returning nil when none is
// configured — which omits the /healthz block entirely rather than rendering an empty
// one, since a zeroed block reads as a broken backup rather than as no backup.
//
// A credential R2 rejects cannot be detected here: the client is constructed locally and
// the first real exchange is the first upload. So a bad secret surfaces as a failure on
// /healthz rather than a refusal to start, which is the right trade — a bucket that has
// gone away should not take the cost API down with it.
func startBackups(ctx context.Context, cfg config.Config, clock testutil.Clock, logger *slog.Logger) httpapi.BackupProvider {
	bc := cfg.Prices.Backup
	if !bc.Configured() {
		// Said out loud, because "I configured backups" and "backups are running"
		// differing silently is the whole failure mode here.
		logger.Info("price archive backups disabled (no prices.backup block configured)")
		return nil
	}

	up, err := backup.NewR2Uploader(backup.R2Config{
		AccountID:       bc.AccountID,
		AccessKeyID:     bc.AccessKeyID,
		SecretAccessKey: bc.SecretAccessKey,
		BucketName:      bc.Bucket,
		DBPath:          cfg.Prices.DBPath,
	})
	if err != nil {
		// Fail open rather than refusing to boot. The archive still collects and every
		// cost route still answers; what is lost is the offsite copy, and /healthz says
		// so. Taking the service down would turn a backup problem into an outage.
		logger.Error("price archive backups unavailable: could not build the R2 uploader",
			"error", err, "bucket", bc.Bucket)
		return nil
	}

	mgr := backup.NewManager(backup.Config{
		DBPath:      cfg.Prices.DBPath,
		BucketName:  bc.Bucket,
		Env:         bc.Env,
		ServiceName: config.BackupServiceName,
		Schedule:    bc.Schedule,
		// Passed as given. The library preserves 0 as midnight and clamps only
		// out-of-range values, so the config layer's default is the only default.
		ScheduleHour: bc.Hour,
		// Injected, so the schedule is the same clock everything else in this service
		// uses — and so a test can drive it without waiting a day.
		Clock: clock.Now,
	}, up, nil)

	mgr.Start(ctx)
	logger.Info("price archive backups enabled",
		"bucket", bc.Bucket, "env", bc.Env, "service", config.BackupServiceName,
		"schedule", effectiveSchedule(bc.Schedule), "hour", bc.Hour,
		"db_path", cfg.Prices.DBPath, "next_run", mgr.NextRun())

	return backupProvider{
		mgr: mgr, bucket: bc.Bucket, env: bc.Env,
		sched: effectiveSchedule(bc.Schedule), hour: bc.Hour,
	}
}

// effectiveSchedule resolves the library's empty-means-daily rule, so /healthz reports
// what will actually happen rather than a blank.
func effectiveSchedule(s string) string {
	if s == "" {
		return "daily"
	}
	return s
}
