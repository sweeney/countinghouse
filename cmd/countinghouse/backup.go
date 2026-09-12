package main

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/sweeney/identity/common/backup"
	_ "modernc.org/sqlite"

	"github.com/sweeney/countinghouse/internal/config"
	"github.com/sweeney/countinghouse/internal/httpapi"
	"github.com/sweeney/countinghouse/internal/testutil"
)

// ---------------------------------------------------------------------------
// Offsite backup of the price archive.
//
// The archive is the one thing countinghouse writes, and the only state here that is
// NOT rebuildable from Influx — CLAUDE.md is explicit that it is primary durable state
// "which is exactly why it gets an enforced key, a restatement log and a backup — not a
// retention policy". This is that backup.
//
// Unconfigured is a legitimate, silent state: a development box, or any deployment whose
// agreements are all flat-rate and which therefore has no archive. A PARTIALLY
// configured one is not — config.BackupConfig.Validate refuses it at startup, because a
// backup that is quietly not happening is worse than none.
//
// WHY THIS DOES NOT USE backup.Manager
//
// It uses `backup.R2Uploader` (the S3-compatible client) but owns the snapshot and the
// schedule, because `Manager` in common@v0.3.0 copies the database with `os.ReadFile` +
// `os.WriteFile`. The archive is WAL-mode, so that uploads the main file with `-wal`
// ignored: the database as of the last checkpoint, staleness unbounded on a quiet
// archive that never reaches the 1000-page auto-checkpoint, and a torn image if a
// checkpoint runs during the read.
//
// `VACUUM INTO` fixes it in one statement and belongs upstream, where it would also fix
// identity and config. Until that is released this file keeps the guarantee locally
// rather than shipping a README sentence the code does not honour. When `common` gains
// `VACUUM INTO` plus a status snapshot and an injected clock, most of this collapses
// back onto `Manager` — and the key layout below is deliberately identical so those
// objects stay restorable by its tooling either way.
// ---------------------------------------------------------------------------

// uploader is the one thing this needs from the R2 client, narrowed so the schedule and
// the snapshot can be tested without credentials or a network.
type uploader interface {
	Upload(ctx context.Context, key, localPath string) error
}

// uploadTimeout bounds a single upload. Generous enough for a large archive over a slow
// link, short enough that a wedged connection does not disable backups indefinitely.
const uploadTimeout = 10 * time.Minute

// archiveBackup snapshots the price archive and uploads it on a schedule.
type archiveBackup struct {
	dbPath   string
	up       uploader
	env      string
	bucket   string
	schedule string
	hour     int
	clock    testutil.Clock
	log      *slog.Logger

	status *backupStatus
}

// snapshotDB writes a consistent, self-contained copy of the SQLite database at src to
// dst, using VACUUM INTO.
//
// Three properties that a file copy does not have: it includes writes still in the WAL,
// it cannot capture a torn page because SQLite serialises it against writers, and the
// result needs no `-wal`/`-shm` companion to open — which matters because the restore
// path is "download one object and open it".
//
// Pure SQL, so the CGO-free build survives. The destination is cleared first: VACUUM
// INTO refuses a path that already exists, and without this the second backup of the
// process's life would fail while the first looked fine.
func snapshotDB(src, dst string) error {
	if _, err := os.Stat(src); err != nil {
		// Checked explicitly: sql.Open is lazy and would CREATE an empty database at a
		// missing path, and uploading an empty archive over a good backup is the worst
		// available outcome.
		return fmt.Errorf("archive not readable at %s: %w", src, err)
	}
	db, err := sql.Open("sqlite", src)
	if err != nil {
		return fmt.Errorf("open archive: %w", err)
	}
	defer db.Close() //nolint:errcheck

	if err := os.Remove(dst); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("clear snapshot path: %w", err)
	}
	if _, err := db.Exec("VACUUM INTO ?", dst); err != nil {
		return fmt.Errorf("vacuum into %s: %w", dst, err)
	}
	// VACUUM INTO creates the file under the process umask. This is a full copy of the
	// price history and the live archive is 0600; the copy must not be looser.
	if err := os.Chmod(dst, 0o600); err != nil {
		return fmt.Errorf("chmod snapshot: %w", err)
	}
	return nil
}

// backupKey is the R2 object key for a backup taken at t.
//
// Format: {env}/backups/{service}/{YYYY/MM/DD}/{service}-{RFC3339}.sqlite3
//
// Byte-for-byte the layout `identity/common/backup` writes and its restore tooling
// matches on — deliberately, so these objects stay restorable by that tooling even
// though this file does the uploading. TestBackupKeyMatchesTheUpstreamLayout pins it;
// changing it silently would strand every prior backup under a prefix nothing looks in.
func backupKey(env, service string, t time.Time) string {
	return fmt.Sprintf("%s/backups/%s/%s/%s-%s.sqlite3",
		env, service, t.Format("2006/01/02"), service, t.Format(time.RFC3339))
}

// RunNow takes a snapshot and uploads it, synchronously.
func (b *archiveBackup) RunNow(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, uploadTimeout)
	defer cancel()

	start := b.clock.Now().UTC()
	key := backupKey(b.env, config.BackupServiceName, start)

	tmp, err := os.CreateTemp("", config.BackupServiceName+"-backup-*.sqlite3")
	if err != nil {
		b.status.record(false, fmt.Sprintf("create temp file: %v", err))
		return fmt.Errorf("create temp file: %w", err)
	}
	path := tmp.Name()
	tmp.Close()           //nolint:errcheck
	defer os.Remove(path) //nolint:errcheck

	if err := snapshotDB(b.dbPath, path); err != nil {
		b.status.record(false, fmt.Sprintf("snapshot: %v", err))
		return fmt.Errorf("snapshot archive: %w", err)
	}
	if err := b.up.Upload(ctx, key, path); err != nil {
		b.status.record(false, fmt.Sprintf("upload: %v", err))
		return fmt.Errorf("upload backup: %w", err)
	}

	b.status.record(true, key)
	if b.log != nil {
		b.log.Info("price archive backed up", "key", key, "bucket", b.bucket,
			"took", b.clock.Now().Sub(start).Round(time.Millisecond))
	}
	return nil
}

// nextRun returns the next scheduled instant strictly after `from`, in UTC.
//
// Strictly after, so a run landing exactly on the scheduled hour schedules the NEXT one
// rather than the same instant again — the difference between a daily backup and a tight
// loop. Hour 0 is midnight, a real setting; the config layer defaults the hour so this
// never has to read 0 as "unset", which is the ambiguity that sends an explicit
// `hour: 0` to 03:00 in common@v0.3.0 while /healthz reports 0.
func (b *archiveBackup) nextRun(from time.Time) (time.Time, bool) {
	if b.schedule == "off" {
		return time.Time{}, false
	}
	from = from.UTC()
	at := time.Date(from.Year(), from.Month(), from.Day(), b.hour, 0, 0, 0, time.UTC)
	for !at.After(from) || !b.scheduledOn(at) {
		at = at.AddDate(0, 0, 1)
	}
	return at, true
}

// scheduledOn reports whether a backup runs on the given day.
func (b *archiveBackup) scheduledOn(at time.Time) bool {
	switch b.schedule {
	case "weekly":
		return at.Weekday() == time.Sunday
	case "monthly":
		return at.Day() == 1
	default: // daily, or empty meaning daily
		return true
	}
}

// Start runs the schedule until ctx is cancelled.
func (b *archiveBackup) Start(ctx context.Context) {
	if b.schedule == "off" {
		b.log.Info("price archive backups are on-demand only (schedule: off)")
		return
	}
	go b.loop(ctx)
}

func (b *archiveBackup) loop(ctx context.Context) {
	for {
		next, ok := b.nextRun(b.clock.Now())
		if !ok {
			return
		}
		// A real timer against a clock-derived deadline, the same shape Collector.Run
		// uses. The CLOCK decides when the next run is due (so the schedule is testable
		// without waiting a day); the timer only does the sleeping.
		timer := time.NewTimer(next.Sub(b.clock.Now()))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		// Fail-open: a failed backup is logged and recorded, and the loop continues to
		// the next scheduled run. One bad night must not disable backups until somebody
		// restarts the service.
		if err := b.RunNow(ctx); err != nil {
			b.log.Error("price archive backup failed", "error", err, "bucket", b.bucket)
		}
	}
}

// BackupHealth implements httpapi.BackupProvider.
func (b *archiveBackup) BackupHealth() *httpapi.BackupHealth { return b.status.BackupHealth() }

// ---------------------------------------------------------------------------

// backupStatus records what the backup has done, for /healthz and /metrics.
//
// Written from the backup goroutine and read by every HTTP request, hence the mutex.
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

// record notes one outcome. On success `detail` is the object key, which is how the
// backup is found later without listing the bucket; on failure it is the error.
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
	// Scrub anything credential-shaped out of an error that is about to be served on
	// /healthz. The AWS SDK's messages do not normally carry the secret, but "normally"
	// is not a property worth betting a credential on, and the error is still useful
	// without whatever this removes.
	b.lastError = redactSecrets(detail)
}

// BackupHealth renders the current state.
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

// redactSecrets removes anything that looks like a credential from text bound for an
// HTTP response.
//
// Deliberately blunt: it drops the remainder of any line mentioning a secret-ish word
// rather than trying to parse the SDK's error formats, because a redactor that
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

// ---------------------------------------------------------------------------

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

	b := newArchiveBackup(cfg, up, clock, logger)
	b.Start(ctx)
	logger.Info("price archive backups enabled",
		"bucket", bc.Bucket, "env", bc.Env, "service", config.BackupServiceName,
		"schedule", b.schedule, "hour", bc.Hour, "db_path", cfg.Prices.DBPath)
	return b
}

// newArchiveBackup assembles the backup from config, an uploader and a clock. Split out
// so the schedule and the snapshot are testable without credentials or a network.
func newArchiveBackup(cfg config.Config, up uploader, clock testutil.Clock, logger *slog.Logger) *archiveBackup {
	bc := cfg.Prices.Backup
	schedule := bc.Schedule
	if schedule == "" {
		// Resolved here rather than left empty so /healthz reports what will actually
		// happen instead of a blank.
		schedule = "daily"
	}
	return &archiveBackup{
		dbPath:   cfg.Prices.DBPath,
		up:       up,
		env:      bc.Env,
		bucket:   bc.Bucket,
		schedule: schedule,
		hour:     bc.Hour,
		clock:    clock,
		log:      logger,
		status: &backupStatus{
			bucket: bc.Bucket, env: bc.Env, schedule: schedule, hour: bc.Hour,
			now: func() time.Time { return clock.Now() },
		},
	}
}
