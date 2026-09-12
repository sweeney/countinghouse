package main

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/sweeney/identity/common/backup"

	"github.com/sweeney/countinghouse/internal/config"
	"github.com/sweeney/countinghouse/internal/httpapi"
)

// ---------------------------------------------------------------------------
// Offsite backup of the price archive.
//
// The archive is the one thing countinghouse writes, and the only state here that
// is NOT rebuildable from Influx — the whole reason to keep it is the day it stops
// being re-fetchable from the supplier. CLAUDE.md is explicit that it is primary
// durable state rather than a cache, "which is exactly why it gets an enforced key,
// a restatement log and a backup — not a retention policy". This is that backup.
//
// Unconfigured is a legitimate, silent state: a development box, or any deployment
// whose agreements are all flat-rate and which therefore has no archive at all. A
// PARTIALLY configured one is not — config.BackupConfig.Validate refuses it at
// startup, because a backup that is quietly not happening is worse than none.
// ---------------------------------------------------------------------------

// backupStatus records what the backup manager has done, for /healthz and /metrics.
//
// The library reports outcomes through a callback rather than exposing state, so
// this is where the two timestamps that matter get kept. It is written from the
// backup goroutine and read by every HTTP request, hence the mutex.
type backupStatus struct {
	mu sync.Mutex

	bucket   string
	env      string
	schedule string
	hour     int

	lastAttempt time.Time
	lastSuccess time.Time
	lastKey     string
	lastError   string
	successes   int
	failures    int

	now func() time.Time
}

// record is the backup.EventRecorder the manager calls on every outcome.
//
// On success `detail` is the object key, which is worth keeping: it is how the
// backup is found later without listing the bucket. On failure it is the error,
// which is worth keeping for the opposite reason.
func (b *backupStatus) record(success bool, detail string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	now := b.now()
	b.lastAttempt = now
	if success {
		b.lastSuccess = now
		b.lastKey = detail
		b.lastError = ""
		b.successes++
		return
	}
	b.failures++
	// Scrub anything credential-shaped out of an error that is about to be served
	// on /healthz. The AWS SDK's messages do not normally carry the secret, but
	// "normally" is not a property worth betting a credential on, and the error is
	// still useful without whatever this removes.
	b.lastError = redactSecrets(detail)
}

// BackupHealth implements httpapi.BackupProvider.
func (b *backupStatus) BackupHealth() *httpapi.BackupHealth {
	b.mu.Lock()
	defer b.mu.Unlock()
	return &httpapi.BackupHealth{
		Bucket:      b.bucket,
		Env:         b.env,
		Schedule:    b.schedule,
		Hour:        b.hour,
		LastAttempt: b.lastAttempt,
		LastSuccess: b.lastSuccess,
		LastKey:     b.lastKey,
		LastError:   b.lastError,
		Successes:   b.successes,
		Failures:    b.failures,
	}
}

// redactSecrets removes anything that looks like a credential from text bound for
// an HTTP response.
//
// Deliberately blunt: it drops the remainder of any line mentioning a secret-ish
// word rather than trying to parse the SDK's error formats, because a redactor that
// understands its input is a redactor that stops working when the input changes.
func redactSecrets(text string) string {
	lower := strings.ToLower(text)
	for _, marker := range []string{"secret", "accesskey", "access key", "access_key", "credential", "authorization", "signature"} {
		if i := strings.Index(lower, marker); i >= 0 {
			return strings.TrimSpace(text[:i]) + " [redacted]"
		}
	}
	return text
}

// startBackups builds and starts the archive's backup manager.
//
// Returns nil when no backup is configured, which callers pass straight to
// httpapi.Server.Backups: a nil provider omits the /healthz block entirely rather
// than rendering an empty one, since a zeroed block reads as a broken backup rather
// than as no backup.
//
// A credential that R2 rejects cannot be detected here — the uploader is
// constructed locally and the first real exchange is the first upload — so the
// consequence of a bad secret is a failure visible on /healthz rather than a
// refusal to start. That is the right trade: a bucket that has gone away should not
// take the cost API down with it.
func startBackups(ctx context.Context, cfg config.Config, logger *slog.Logger) httpapi.BackupProvider {
	bc := cfg.Prices.Backup
	if !bc.Configured() {
		// Said out loud, because "I configured backups" and "backups are running"
		// differing silently is the whole failure mode here.
		logger.Info("price archive backups disabled (no prices.backup block configured)")
		return nil
	}

	uploader, err := backup.NewR2Uploader(backup.R2Config{
		AccountID:       bc.AccountID,
		AccessKeyID:     bc.AccessKeyID,
		SecretAccessKey: bc.SecretAccessKey,
		BucketName:      bc.Bucket,
		DBPath:          cfg.Prices.DBPath,
	})
	if err != nil {
		// Fail open rather than refusing to boot. The archive still collects and
		// every cost route still answers; what is lost is the offsite copy, and
		// /healthz says so. Taking the service down would convert a backup problem
		// into an outage.
		logger.Error("price archive backups unavailable: could not build the R2 uploader",
			"error", err, "bucket", bc.Bucket)
		return nil
	}

	status := &backupStatus{
		bucket:   bc.Bucket,
		env:      bc.Env,
		schedule: orDaily(bc.Schedule),
		hour:     bc.Hour,
		now:      time.Now,
	}

	manager := backup.NewManager(backup.Config{
		DBPath:      cfg.Prices.DBPath,
		BucketName:  bc.Bucket,
		Env:         bc.Env,
		ServiceName: config.BackupServiceName,
		Schedule:    bc.Schedule,
		// Hour is passed as given. The library treats 0 as midnight rather than as
		// unset — which is why config.Default fills it in, so an unset key here is
		// already the documented default and not an accidental midnight.
		ScheduleHour: bc.Hour,
	}, uploader, status.record)

	manager.Start(ctx)
	logger.Info("price archive backups enabled",
		"bucket", bc.Bucket, "env", bc.Env, "service", config.BackupServiceName,
		"schedule", status.schedule, "hour", bc.Hour, "db_path", cfg.Prices.DBPath)
	return status
}

// orDaily reports the effective schedule, resolving the library's empty-means-daily
// rule so /healthz shows what will actually happen rather than an empty string.
func orDaily(schedule string) string {
	if schedule == "" {
		return "daily"
	}
	return schedule
}
