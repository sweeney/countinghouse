package main

import (
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/sweeney/countinghouse/internal/config"
)

// ---------------------------------------------------------------------------
// The backup status tracker.
//
// The library reports outcomes through a callback and keeps no state, so this is
// the only place that knows whether backups are working. Its two timestamps are
// what /healthz turns into a verdict, and the distinction between them — attempted
// versus succeeded — is the entire signal.
// ---------------------------------------------------------------------------

// trackerAt builds a status whose clock is fixed, so elapsed time is an assertion
// rather than a race.
func trackerAt(t time.Time) *backupStatus {
	return &backupStatus{
		bucket: "countinghouse-sqlite", env: config.EnvProduction,
		schedule: "daily", hour: 3,
		now: func() time.Time { return t },
	}
}

// Before anything has run, both timestamps are zero — which is how /healthz tells
// "not yet" from "never worked" and declines to complain about a fresh start.
func TestBackupStatusStartsEmpty(t *testing.T) {
	h := trackerAt(time.Now()).BackupHealth()
	if !h.LastAttempt.IsZero() || !h.LastSuccess.IsZero() {
		t.Errorf("a fresh tracker reports timestamps: %+v", h)
	}
	if h.Successes != 0 || h.Failures != 0 {
		t.Errorf("a fresh tracker reports counts: %+v", h)
	}
	if h.Bucket == "" || h.Env == "" {
		t.Error("the destination must be reported even before the first run")
	}
}

func TestBackupStatusRecordsSuccess(t *testing.T) {
	at := time.Date(2026, 9, 13, 3, 0, 0, 0, time.UTC)
	s := trackerAt(at)
	const key = "production/backups/countinghouse/2026/09/13/countinghouse-x.sqlite3"
	s.record(true, key)

	h := s.BackupHealth()
	if !h.LastSuccess.Equal(at) || !h.LastAttempt.Equal(at) {
		t.Errorf("timestamps = %v / %v, want both %v", h.LastAttempt, h.LastSuccess, at)
	}
	// The key is kept because it is how the backup is found later without listing
	// the bucket — which matters most in the situation where you need it.
	if h.LastKey != key {
		t.Errorf("LastKey = %q, want the object key", h.LastKey)
	}
	if h.Successes != 1 || h.Failures != 0 {
		t.Errorf("counts = %d/%d, want 1/0", h.Successes, h.Failures)
	}
}

// A failure moves LastAttempt but NOT LastSuccess. Moving both would erase the one
// fact that matters: how long it has been since a backup actually worked.
func TestBackupStatusFailureDoesNotAdvanceLastSuccess(t *testing.T) {
	good := time.Date(2026, 9, 13, 3, 0, 0, 0, time.UTC)
	s := trackerAt(good)
	s.record(true, "a-key")

	bad := good.Add(24 * time.Hour)
	s.now = func() time.Time { return bad }
	s.record(false, "upload backup: connection reset")

	h := s.BackupHealth()
	if !h.LastSuccess.Equal(good) {
		t.Errorf("LastSuccess = %v, want it held at %v — a failure is not a success", h.LastSuccess, good)
	}
	if !h.LastAttempt.Equal(bad) {
		t.Errorf("LastAttempt = %v, want %v", h.LastAttempt, bad)
	}
	if h.LastError == "" {
		t.Error("the error must be reported")
	}
	// The successful key survives: it is still the newest backup that exists.
	if h.LastKey != "a-key" {
		t.Errorf("LastKey = %q; the last good backup is still the last good backup", h.LastKey)
	}
	if h.Successes != 1 || h.Failures != 1 {
		t.Errorf("counts = %d/%d, want 1/1", h.Successes, h.Failures)
	}
}

// Recovery clears the error. Leaving it set would keep /healthz degraded forever
// after one bad night, which trains an operator to ignore it.
func TestBackupStatusSuccessClearsTheError(t *testing.T) {
	s := trackerAt(time.Date(2026, 9, 13, 3, 0, 0, 0, time.UTC))
	s.record(false, "upload backup: 503")
	s.record(true, "a-key")
	if got := s.BackupHealth().LastError; got != "" {
		t.Errorf("LastError = %q, want cleared after a success", got)
	}
}

// ---------------------------------------------------------------------------

// The error text is served on /healthz, so anything credential-shaped must not
// survive. The redactor is deliberately blunt — it truncates at the marker rather
// than parsing the SDK's formats, because a redactor that understands its input
// stops working when the input changes.
func TestRedactSecrets(t *testing.T) {
	tests := []struct {
		name   string
		in     string
		reject string
	}{
		{"an ordinary error passes through", "upload backup: connection reset by peer", ""},
		{"a secret is cut", "signing failed with SecretAccessKey=abc123def", "abc123def"},
		{"lowercase secret", "bad secret_access_key: abc123def", "abc123def"},
		{"an access key id", "InvalidAccessKeyId: AKIAIOSFODNN7EXAMPLE", "AKIAIOSFODNN7EXAMPLE"},
		{"an authorization header", "Authorization: AWS4-HMAC-SHA256 Credential=abc/xyz", "abc/xyz"},
		{"a signature", "signature mismatch: computed 9f86d081884c7d", "9f86d081884c7d"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := redactSecrets(tc.in)
			if tc.reject == "" {
				if got != tc.in {
					t.Errorf("redactSecrets(%q) = %q, want it unchanged", tc.in, got)
				}
				return
			}
			if strings.Contains(got, tc.reject) {
				t.Errorf("redactSecrets(%q) = %q, still contains %q", tc.in, got, tc.reject)
			}
			if got == "" {
				t.Error("redaction emptied the message; the error is still worth reporting")
			}
		})
	}
}

// The redactor runs on the recorded error, not just where it is called — otherwise
// it is one refactor away from being bypassed.
func TestBackupStatusRedactsTheRecordedError(t *testing.T) {
	s := trackerAt(time.Now())
	s.record(false, "auth failed, SecretAccessKey=super-secret-value")
	if strings.Contains(s.BackupHealth().LastError, "super-secret-value") {
		t.Errorf("the recorded error carries the secret: %q", s.BackupHealth().LastError)
	}
}

// ---------------------------------------------------------------------------

// No backup block configured returns a nil provider, which is what omits the
// /healthz block. Returning an empty tracker instead would render a zeroed block
// that reads as a broken backup on every development box.
func TestStartBackupsUnconfiguredReturnsNil(t *testing.T) {
	cfg := config.Default()
	cfg.Prices.DBPath = "/tmp/prices.db"
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	if p := startBackups(t.Context(), cfg, logger); p != nil {
		t.Errorf("an unconfigured deployment got a provider: %+v", p)
	}
}

// Configured, it returns a live provider reporting the destination. The uploader is
// built locally and never contacts R2 until the first upload, so this does not need
// credentials that work — which is also why a bad secret surfaces on /healthz rather
// than as a refusal to start.
func TestStartBackupsConfiguredReportsItsDestination(t *testing.T) {
	cfg := config.Default()
	cfg.Prices.DBPath = "/tmp/prices.db"
	cfg.Prices.Backup = config.BackupConfig{
		Env: config.EnvProduction, Bucket: "countinghouse-sqlite",
		AccountID: "acct", AccessKeyID: "akid", SecretAccessKey: "secret",
		Schedule: "", Hour: 3,
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	p := startBackups(t.Context(), cfg, logger)
	if p == nil {
		t.Fatal("a configured deployment got no provider")
	}
	h := p.BackupHealth()
	if h.Bucket != "countinghouse-sqlite" || h.Env != config.EnvProduction {
		t.Errorf("destination = %s/%s", h.Env, h.Bucket)
	}
	// An empty schedule must be reported as what will actually happen, not as "".
	if h.Schedule != "daily" {
		t.Errorf("Schedule = %q, want daily — the library's empty-means-daily resolved", h.Schedule)
	}
	// Nothing has run, so the verdict side must see a clean slate.
	if !h.LastAttempt.IsZero() || h.Failures != 0 {
		t.Errorf("a just-started provider reports activity: %+v", h)
	}
}
