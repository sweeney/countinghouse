package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sweeney/countinghouse/internal/config"
	"github.com/sweeney/countinghouse/internal/testutil"
)

// ---------------------------------------------------------------------------
// The object key, and when backups run.
// ---------------------------------------------------------------------------

// The key layout is a CONTRACT with identity/common/backup's restore tooling, which
// matches on `{env}/backups/{service}/` and parses the rest. countinghouse does its own
// uploading (see backup.go for why), so nothing but this test stops the two drifting —
// and a drift strands every prior backup under a prefix nothing looks in.
func TestBackupKeyMatchesTheUpstreamLayout(t *testing.T) {
	at := time.Date(2026, 9, 13, 3, 0, 0, 0, time.UTC)
	got := backupKey("production", "countinghouse", at)
	const want = "production/backups/countinghouse/2026/09/13/countinghouse-2026-09-13T03:00:00Z.sqlite3"
	if got != want {
		t.Errorf("backupKey =\n  %s\nwant\n  %s", got, want)
	}

	// The pieces the restore tooling relies on, asserted separately so a failure says
	// which part moved.
	if !strings.HasPrefix(got, "production/backups/countinghouse/") {
		t.Error("the prefix must be {env}/backups/{service}/ — it is what a restore lists on")
	}
	if !strings.HasSuffix(got, ".sqlite3") {
		t.Error("the extension must be .sqlite3")
	}
	// Date path segments are zero-padded, so lexical order is chronological order.
	if !strings.Contains(got, "/2026/09/13/") {
		t.Error("the date path must be zero-padded YYYY/MM/DD so keys sort chronologically")
	}
}

// Keys must be UTC, so two backups either side of a DST change sort correctly and a
// restore does not have to guess a timezone.
func TestBackupKeyIsUTC(t *testing.T) {
	loc, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	// 00:30 BST on 1 July is 23:30 UTC on 30 June — a different DAY.
	local := time.Date(2026, 7, 1, 0, 30, 0, 0, loc)
	got := backupKey("production", "countinghouse", local.UTC())
	if !strings.Contains(got, "/2026/06/30/") {
		t.Errorf("key = %s; it must use the UTC date, or a backup taken just after local "+
			"midnight files under the wrong day", got)
	}
	if !strings.Contains(got, "T23:30:00Z") {
		t.Errorf("key = %s; the timestamp must be UTC with a Z", got)
	}
}

// ---------------------------------------------------------------------------

// testBackup builds an archiveBackup with a fake clock and a recording uploader.
func testBackup(t *testing.T, schedule string, hour int, now time.Time) (*archiveBackup, *fakeUploader, *testutil.FakeClock) {
	t.Helper()
	cfg := config.Default()
	cfg.Prices.DBPath = "/nonexistent/prices.db" // never actually snapshotted in these tests
	cfg.Prices.Backup = config.BackupConfig{
		Env: config.EnvProduction, Bucket: "countinghouse-sqlite",
		AccountID: "a", AccessKeyID: "k", SecretAccessKey: "s",
		Schedule: schedule, Hour: hour,
	}
	up := &fakeUploader{}
	clock := testutil.NewFakeClock(now)
	return newArchiveBackup(cfg, up, clock, slog.New(slog.NewTextHandler(io.Discard, nil))), up, clock
}

// fakeUploader records keys and can be made to fail.
type fakeUploader struct {
	keys []string
	err  error
}

func (f *fakeUploader) Upload(_ context.Context, key, _ string) error {
	f.keys = append(f.keys, key)
	return f.err
}

func TestNextRunDaily(t *testing.T) {
	// 02:00 UTC, with backups due at 03:00 — later today.
	b, _, _ := testBackup(t, "daily", 3, time.Date(2026, 9, 13, 2, 0, 0, 0, time.UTC))
	next, ok := b.nextRun(b.clock.Now())
	if !ok {
		t.Fatal("a daily schedule reported nothing due")
	}
	if want := time.Date(2026, 9, 13, 3, 0, 0, 0, time.UTC); !next.Equal(want) {
		t.Errorf("next = %s, want %s", next, want)
	}

	// 04:00, past today's slot — tomorrow.
	b2, _, _ := testBackup(t, "daily", 3, time.Date(2026, 9, 13, 4, 0, 0, 0, time.UTC))
	next2, _ := b2.nextRun(b2.clock.Now())
	if want := time.Date(2026, 9, 14, 3, 0, 0, 0, time.UTC); !next2.Equal(want) {
		t.Errorf("next = %s, want %s", next2, want)
	}
}

// Exactly on the scheduled instant must schedule the NEXT one, not the same instant
// again — the difference between a daily backup and a tight loop.
func TestNextRunAtExactlyTheScheduledInstant(t *testing.T) {
	b, _, _ := testBackup(t, "daily", 3, time.Date(2026, 9, 13, 3, 0, 0, 0, time.UTC))
	next, _ := b.nextRun(b.clock.Now())
	if want := time.Date(2026, 9, 14, 3, 0, 0, 0, time.UTC); !next.Equal(want) {
		t.Errorf("next = %s, want %s — a run landing on the scheduled hour must schedule "+
			"tomorrow, or the loop spins", next, want)
	}
}

// Hour 0 is MIDNIGHT, a real setting — not "unset". common@v0.3.0 reads 0 as unset and
// silently runs at 03:00 while /healthz reports 0, which is the absent-vs-zero confusion
// this branch removes everywhere else.
func TestNextRunAtMidnight(t *testing.T) {
	b, _, _ := testBackup(t, "daily", 0, time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC))
	next, _ := b.nextRun(b.clock.Now())
	want := time.Date(2026, 9, 14, 0, 0, 0, 0, time.UTC)
	if !next.Equal(want) {
		t.Errorf("next = %s, want %s — hour 0 means midnight", next, want)
	}
	if next.Hour() == 3 {
		t.Error("hour 0 was treated as unset and defaulted to 03:00")
	}
}

func TestNextRunWeekly(t *testing.T) {
	// Monday 14 September 2026. The next Sunday is the 20th.
	b, _, _ := testBackup(t, "weekly", 3, time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC))
	next, ok := b.nextRun(b.clock.Now())
	if !ok {
		t.Fatal("weekly reported nothing due")
	}
	if next.Weekday() != time.Sunday {
		t.Errorf("next = %s (%s), want a Sunday", next, next.Weekday())
	}
	if want := time.Date(2026, 9, 20, 3, 0, 0, 0, time.UTC); !next.Equal(want) {
		t.Errorf("next = %s, want %s", next, want)
	}
}

func TestNextRunMonthly(t *testing.T) {
	b, _, _ := testBackup(t, "monthly", 3, time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC))
	next, ok := b.nextRun(b.clock.Now())
	if !ok {
		t.Fatal("monthly reported nothing due")
	}
	if want := time.Date(2026, 10, 1, 3, 0, 0, 0, time.UTC); !next.Equal(want) {
		t.Errorf("next = %s, want %s (the 1st)", next, want)
	}
}

// "off" means the timer never fires. On-demand still works — that is what makes it
// different from not configuring backups at all.
func TestNextRunOff(t *testing.T) {
	b, _, _ := testBackup(t, "off", 3, time.Date(2026, 9, 13, 2, 0, 0, 0, time.UTC))
	if _, ok := b.nextRun(b.clock.Now()); ok {
		t.Error("schedule off reported a due time")
	}
	// And Start must not leave a goroutine waiting on a zero deadline.
	b.Start(context.Background())
}

// An empty schedule resolves to daily in ONE place, so /healthz reports what will
// actually happen rather than a blank.
func TestEmptyScheduleIsDaily(t *testing.T) {
	b, _, _ := testBackup(t, "", 3, time.Date(2026, 9, 13, 2, 0, 0, 0, time.UTC))
	if b.schedule != "daily" {
		t.Errorf("schedule = %q, want daily", b.schedule)
	}
	if b.BackupHealth().Schedule != "daily" {
		t.Errorf("/healthz schedule = %q, want daily", b.BackupHealth().Schedule)
	}
	if _, ok := b.nextRun(b.clock.Now()); !ok {
		t.Error("an empty schedule reported nothing due")
	}
}

// ---------------------------------------------------------------------------

// A failed snapshot must be recorded and reported, and must not upload anything —
// uploading an empty or partial file over a good backup is the worst outcome available.
func TestRunNowRecordsASnapshotFailureAndUploadsNothing(t *testing.T) {
	b, up, _ := testBackup(t, "daily", 3, time.Date(2026, 9, 13, 3, 0, 0, 0, time.UTC))

	if err := b.RunNow(context.Background()); err == nil {
		t.Fatal("RunNow succeeded with an unreadable archive")
	}
	if len(up.keys) != 0 {
		t.Errorf("uploaded %v despite the snapshot failing", up.keys)
	}
	h := b.BackupHealth()
	if h.Failures != 1 {
		t.Errorf("failures = %d, want 1", h.Failures)
	}
	if !h.LastSuccess.IsZero() {
		t.Error("a failure must not advance last_success")
	}
	if h.LastError == "" {
		t.Error("the failure must be reported on /healthz")
	}
}

// An upload failure is recorded too, and the error reaches /healthz redacted.
func TestRunNowRecordsAnUploadFailure(t *testing.T) {
	src := archiveWithUncheckpointedWrites(t, 10)
	b, up, _ := testBackup(t, "daily", 3, time.Date(2026, 9, 13, 3, 0, 0, 0, time.UTC))
	b.dbPath = src
	up.err = errors.New("AccessDenied: SecretAccessKey=hunter2")

	if err := b.RunNow(context.Background()); err == nil {
		t.Fatal("RunNow succeeded despite the upload failing")
	}
	h := b.BackupHealth()
	if h.Failures != 1 {
		t.Errorf("failures = %d, want 1", h.Failures)
	}
	if strings.Contains(h.LastError, "hunter2") {
		t.Errorf("the credential reached /healthz: %q", h.LastError)
	}
}

// The success path end to end: a real archive, snapshotted, uploaded under the expected
// key, recorded.
func TestRunNowUploadsASnapshot(t *testing.T) {
	src := archiveWithUncheckpointedWrites(t, 100)
	at := time.Date(2026, 9, 13, 3, 0, 0, 0, time.UTC)
	b, up, _ := testBackup(t, "daily", 3, at)
	b.dbPath = src

	if err := b.RunNow(context.Background()); err != nil {
		t.Fatalf("RunNow: %v", err)
	}
	if len(up.keys) != 1 {
		t.Fatalf("uploaded %d objects, want 1", len(up.keys))
	}
	want := backupKey(config.EnvProduction, config.BackupServiceName, at)
	if up.keys[0] != want {
		t.Errorf("key = %s, want %s", up.keys[0], want)
	}

	h := b.BackupHealth()
	if h.Successes != 1 || h.Failures != 0 {
		t.Errorf("counts = %d/%d, want 1/0", h.Successes, h.Failures)
	}
	if h.LastKey != want {
		t.Errorf("last_key = %q, want the uploaded key", h.LastKey)
	}
	if !h.LastSuccess.Equal(at) {
		t.Errorf("last_success = %s, want the clock's instant %s — the timestamp must come "+
			"from the injected clock, not from time.Now", h.LastSuccess, at)
	}
}

// The temp snapshot is deleted after the upload. It is a full copy of the price history,
// and leaving one behind per run fills the disk with them.
func TestRunNowCleansUpItsSnapshot(t *testing.T) {
	src := archiveWithUncheckpointedWrites(t, 10)
	b, _, _ := testBackup(t, "daily", 3, time.Date(2026, 9, 13, 3, 0, 0, 0, time.UTC))
	b.dbPath = src

	before := countTempSnapshots(t)
	if err := b.RunNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	if after := countTempSnapshots(t); after != before {
		t.Errorf("temp snapshots went from %d to %d; each run must clean up after itself",
			before, after)
	}
}

// countTempSnapshots counts leftover snapshot temp files.
func countTempSnapshots(t *testing.T) int {
	t.Helper()
	entries, err := filepath.Glob(filepath.Join(os.TempDir(), config.BackupServiceName+"-backup-*.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	return len(entries)
}
