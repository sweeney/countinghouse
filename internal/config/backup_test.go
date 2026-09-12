package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Backup configuration.
//
// The price archive is the one thing this service writes, and the whole reason to
// keep it is the day it stops being re-fetchable from the supplier. A backup that
// is quietly not happening is therefore worse than no backup at all: the operator
// believes the archive is safe, and finds out otherwise at the only moment it
// matters.
//
// So every partial configuration is REFUSED at startup rather than degraded into
// doing nothing. The one exception is no configuration at all, which is an explicit
// choice (dev, or a flat-rate deployment with no archive) and runs silently.
// ---------------------------------------------------------------------------

// fullBackup is a valid configuration, for tests that remove one field at a time.
func fullBackup() BackupConfig {
	return BackupConfig{
		Env:             EnvProduction,
		Bucket:          "countinghouse-sqlite",
		AccountID:       "acct",
		AccessKeyID:     "akid",
		SecretAccessKey: "secret",
		Schedule:        "daily",
		Hour:            3,
	}
}

// Nothing configured is the documented "backups off" state, not an error. A
// development box and a flat-rate deployment both legitimately want it.
func TestBackupUnconfiguredIsLegalAndOff(t *testing.T) {
	var b BackupConfig
	if b.Configured() {
		t.Error("an empty block reports itself configured")
	}
	if err := b.Validate("/var/lib/countinghouse/prices.db"); err != nil {
		t.Errorf("an empty block must be legal: %v", err)
	}
}

func TestBackupFullConfigurationIsValid(t *testing.T) {
	b := fullBackup()
	if !b.Configured() {
		t.Error("a complete block does not report itself configured")
	}
	if err := b.Validate("/var/lib/countinghouse/prices.db"); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

// Each credential field missing on its own. Every one of these would produce a
// service that starts cleanly, logs nothing alarming, and never uploads — which is
// the failure this refusal exists to prevent.
func TestBackupPartialConfigurationIsRefused(t *testing.T) {
	tests := []struct {
		name  string
		strip func(*BackupConfig)
		want  string
	}{
		{"no bucket", func(b *BackupConfig) { b.Bucket = "" }, "bucket"},
		{"no account id", func(b *BackupConfig) { b.AccountID = "" }, "account_id"},
		{"no access key id", func(b *BackupConfig) { b.AccessKeyID = "" }, "access_key_id"},
		{"no secret", func(b *BackupConfig) { b.SecretAccessKey = "" }, "secret_access_key"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := fullBackup()
			tc.strip(&b)
			err := b.Validate("/var/lib/countinghouse/prices.db")
			if err == nil {
				t.Fatalf("a block missing %s was accepted; it would never upload", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error does not name the missing field %q: %v", tc.want, err)
			}
			// And it must still count as configured, or the caller would treat it as
			// the legitimate "off" case and never surface the error at all.
			if !b.Configured() {
				t.Error("a partially-filled block must count as configured so it can be refused")
			}
		})
	}
}

// env is not a free-form label: it is the R2 key prefix, and the restore tooling
// matches on the literals "production" and "development". A typo would write
// backups to a prefix no restore looks in — present in the bucket, invisible to
// recovery.
func TestBackupEnvMustBeOneOfTwoLiterals(t *testing.T) {
	for _, env := range []string{"prod", "PRODUCTION", "live", "staging", "Production"} {
		b := fullBackup()
		b.Env = env
		err := b.Validate("/var/lib/countinghouse/prices.db")
		if err == nil {
			t.Errorf("env %q was accepted; backups would land where no restore looks", env)
			continue
		}
		if !strings.Contains(err.Error(), "env") {
			t.Errorf("env %q: error does not mention env: %v", env, err)
		}
	}
	for _, env := range []string{EnvProduction, EnvDevelopment} {
		b := fullBackup()
		b.Env = env
		if err := b.Validate("/var/lib/countinghouse/prices.db"); err != nil {
			t.Errorf("env %q must be accepted: %v", env, err)
		}
	}
}

// An unset env is refused rather than defaulted. The library defaults it to
// "development", so a production deployment that forgot the key would file its
// backups under a development prefix — they exist, and a production restore cannot
// see them. Defaulting the other way is no better: a dev box would pollute the
// production prefix. There is no safe default, so it must be stated.
func TestBackupEnvMustBeStatedExplicitly(t *testing.T) {
	b := fullBackup()
	b.Env = ""
	err := b.Validate("/var/lib/countinghouse/prices.db")
	if err == nil {
		t.Fatal("an unset env was accepted; it would silently become 'development'")
	}
	if !strings.Contains(err.Error(), "env") {
		t.Errorf("error does not mention env: %v", err)
	}
}

// Backups configured with no archive to back up. The operator believes the archive
// is protected and there is no archive — almost always a half-finished edit.
func TestBackupWithoutAnArchiveIsRefused(t *testing.T) {
	b := fullBackup()
	err := b.Validate("")
	if err == nil {
		t.Fatal("backups were configured with no db_path; there is nothing to upload")
	}
	if !strings.Contains(err.Error(), "db_path") {
		t.Errorf("error does not name db_path: %v", err)
	}
}

// The schedule vocabulary is the library's. An unrecognised value would be passed
// straight through and silently treated as "daily", so the operator who wrote
// "nightly" or "hourly" gets neither what they asked for nor a complaint.
func TestBackupScheduleVocabulary(t *testing.T) {
	for _, s := range []string{"daily", "weekly", "monthly", "off"} {
		b := fullBackup()
		b.Schedule = s
		if err := b.Validate("/var/lib/countinghouse/prices.db"); err != nil {
			t.Errorf("schedule %q must be accepted: %v", s, err)
		}
	}
	for _, s := range []string{"hourly", "nightly", "DAILY", "yearly"} {
		b := fullBackup()
		b.Schedule = s
		if err := b.Validate("/var/lib/countinghouse/prices.db"); err == nil {
			t.Errorf("schedule %q was accepted and would silently become daily", s)
		}
	}
	// Unset is fine: the library's own default is daily, which is the right answer
	// for a store whose loss is the thing being insured against.
	b := fullBackup()
	b.Schedule = ""
	if err := b.Validate("/var/lib/countinghouse/prices.db"); err != nil {
		t.Errorf("an unset schedule must default rather than refuse: %v", err)
	}
}

// Hour 0 is midnight UTC, a real setting, so it must not be read as "unset" — which
// is why the config default is stated in Default() rather than inferred from zero.
func TestBackupHourRange(t *testing.T) {
	for _, h := range []int{0, 3, 23} {
		b := fullBackup()
		b.Hour = h
		if err := b.Validate("/var/lib/countinghouse/prices.db"); err != nil {
			t.Errorf("hour %d must be accepted: %v", h, err)
		}
	}
	for _, h := range []int{-1, 24, 99} {
		b := fullBackup()
		b.Hour = h
		if err := b.Validate("/var/lib/countinghouse/prices.db"); err == nil {
			t.Errorf("hour %d was accepted; the library would clamp it to 3 without saying so", h)
		}
	}
}

// The error must not quote the secret. A config error is logged at startup, and a
// startup log is the least private place in the deployment.
func TestBackupErrorsDoNotLeakTheSecret(t *testing.T) {
	const secret = "r2-secret-value-do-not-log"
	b := fullBackup()
	b.SecretAccessKey = secret
	b.Env = "nonsense"
	err := b.Validate("/var/lib/countinghouse/prices.db")
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("the validation error quotes the secret: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Load-level behaviour: the secret file, and refusing at startup.
// ---------------------------------------------------------------------------

// The secret belongs in a file, not in a document that is otherwise harmless to
// copy around — the same reasoning as influx.token_file, and the same mechanism.
func TestLoadReadsTheR2SecretFromAFile(t *testing.T) {
	dir := t.TempDir()
	secretPath := filepath.Join(dir, "r2-secret")
	// A trailing newline is what any editor or `echo` leaves behind, and a secret
	// with a stray \n fails authentication in a way that looks like a wrong key.
	if err := os.WriteFile(secretPath, []byte("the-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(writeConfig(t, `prices:
  db_path: "/var/lib/countinghouse/prices.db"
  backup:
    env: "production"
    bucket: "countinghouse-sqlite"
    account_id: "acct"
    access_key_id: "akid"
    secret_access_key_file: "`+secretPath+`"
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Prices.Backup.SecretAccessKey; got != "the-secret" {
		t.Errorf("secret = %q, want %q with the trailing newline stripped", got, "the-secret")
	}
}

// An inline secret wins, so an operator can override a deployed file without
// editing it — matching influx.token, where the file is read only when the inline
// value is empty.
func TestLoadPrefersTheInlineSecret(t *testing.T) {
	dir := t.TempDir()
	secretPath := filepath.Join(dir, "r2-secret")
	if err := os.WriteFile(secretPath, []byte("from-file"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(writeConfig(t, `prices:
  db_path: "/tmp/prices.db"
  backup:
    env: "development"
    bucket: "b"
    account_id: "a"
    access_key_id: "k"
    secret_access_key: "inline"
    secret_access_key_file: "`+secretPath+`"
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Prices.Backup.SecretAccessKey != "inline" {
		t.Errorf("secret = %q, want the inline value", cfg.Prices.Backup.SecretAccessKey)
	}
}

// A named secret file that is not there is a refusal, not a warning. It is the
// shape of a deployment where the secret was never installed, and starting anyway
// means backups that silently never happen.
func TestLoadRefusesAMissingSecretFile(t *testing.T) {
	_, err := Load(writeConfig(t, `prices:
  db_path: "/tmp/prices.db"
  backup:
    env: "production"
    bucket: "b"
    account_id: "a"
    access_key_id: "k"
    secret_access_key_file: "/nonexistent/r2-secret"
`))
	if err == nil {
		t.Fatal("a missing secret file was accepted")
	}
	if !strings.Contains(err.Error(), "/nonexistent/r2-secret") {
		t.Errorf("error does not name the path: %v", err)
	}
}

// An empty secret file fails every upload with an auth error hours later. Catch it
// at startup, where the cause is still obvious.
func TestLoadRefusesAnEmptySecretFile(t *testing.T) {
	dir := t.TempDir()
	secretPath := filepath.Join(dir, "r2-secret")
	if err := os.WriteFile(secretPath, []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(writeConfig(t, `prices:
  db_path: "/tmp/prices.db"
  backup:
    env: "production"
    bucket: "b"
    account_id: "a"
    access_key_id: "k"
    secret_access_key_file: "`+secretPath+`"
`))
	if err == nil {
		t.Fatal("an empty secret file was accepted")
	}
	if !strings.Contains(err.Error(), "empty") {
		t.Errorf("error does not say the file is empty: %v", err)
	}
}

// A half-configured block must fail Load, not just Validate in isolation —
// otherwise the refusal depends on someone remembering to call it.
func TestLoadRefusesAPartialBackupBlock(t *testing.T) {
	_, err := Load(writeConfig(t, `prices:
  db_path: "/tmp/prices.db"
  backup:
    bucket: "countinghouse-sqlite"
`))
	if err == nil {
		t.Fatal("Load accepted a backup block with only a bucket")
	}
	if !strings.Contains(err.Error(), "account_id") {
		t.Errorf("error does not list what is missing: %v", err)
	}
}

// No backup block at all still loads. Development and flat-rate deployments both
// want this, and it must stay the quiet path.
func TestLoadWithoutABackupBlock(t *testing.T) {
	cfg, err := Load(writeConfig(t, `prices:
  db_path: "/tmp/prices.db"
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Prices.Backup.Configured() {
		t.Error("an absent backup block reports itself configured")
	}
	// The hour is still defaulted, because 0 would otherwise be indistinguishable
	// from a deliberate midnight if backups were later switched on.
	if cfg.Prices.Backup.Hour != DefaultBackupHour {
		t.Errorf("Hour = %d, want the default %d", cfg.Prices.Backup.Hour, DefaultBackupHour)
	}
}

// ---------------------------------------------------------------------------

// The shipped example config must LOAD, and its commented-out backup block must
// load once uncommented.
//
// A template nobody executes is a template that rots: the block is the only
// instructions an operator has, and a wrong indent or a stale key name turns
// "follow the example" into a startup refusal at the worst moment. Uncommenting it
// here is what makes the documentation a test.
func TestExampleConfigLoadsWithAndWithoutTheBackupBlock(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "config", "config.example.yaml"))
	if err != nil {
		t.Fatalf("read example config: %v", err)
	}

	// The example points influx at a token file that only exists on a deployed host.
	// Fill the inline token and blank the path — replacing the path line with a
	// second `token:` would be a duplicate key, which YAML rejects.
	inline := strings.NewReplacer(
		"token: \"\"", "token: \"t\"",
		"token_file: \"/etc/countinghouse/influx-token\"", "token_file: \"\"",
	).Replace(string(raw))

	t.Run("as shipped", func(t *testing.T) {
		cfg, err := Load(writeConfig(t, inline))
		if err != nil {
			t.Fatalf("the example config does not load: %v", err)
		}
		if cfg.Prices.Backup.Configured() {
			t.Error("the example ships with backups enabled; the block must be commented out")
		}
	})

	t.Run("with the backup block uncommented", func(t *testing.T) {
		// Uncomment exactly the prices.backup block: its lines are "  # " followed by
		// content already carrying the right indentation relative to `prices:`.
		var out []string
		inBackup := false
		for _, line := range strings.Split(inline, "\n") {
			if strings.HasPrefix(line, "  # backup:") {
				inBackup = true
			} else if inBackup && !strings.HasPrefix(line, "  #") && strings.TrimSpace(line) != "" {
				inBackup = false
			}
			if inBackup && strings.HasPrefix(line, "  # ") {
				line = "  " + strings.TrimPrefix(line, "  # ")
			}
			out = append(out, line)
		}
		doc := strings.Join(out, "\n")
		if !strings.Contains(doc, "\n  backup:\n") {
			t.Fatalf("the uncommenting did not find the block; the example's shape has changed:\n%s", doc)
		}

		// The example leaves credentials as REPLACE_ME and points the secret at a
		// deployed file, so stand in for both — the point here is that the KEYS and
		// INDENTATION parse into the struct, not that the placeholders are usable.
		doc = strings.NewReplacer(
			"secret_access_key_file: \"/etc/countinghouse/r2-secret\"", "secret_access_key_file: \"\"",
			"secret_access_key: \"\"", "secret_access_key: \"s\"",
		).Replace(doc)

		cfg, err := Load(writeConfig(t, doc))
		if err != nil {
			t.Fatalf("the example's backup block does not load when uncommented: %v", err)
		}
		b := cfg.Prices.Backup
		if !b.Configured() {
			t.Fatal("the uncommented block did not parse into BackupConfig")
		}
		// Every documented key must actually have landed — a typo'd key in YAML is
		// silently ignored, which is how a template comes to configure nothing.
		if b.Env != EnvProduction {
			t.Errorf("env = %q, want %q", b.Env, EnvProduction)
		}
		if b.Bucket == "" {
			t.Error("bucket did not parse")
		}
		if b.AccountID == "" {
			t.Error("account_id did not parse")
		}
		if b.AccessKeyID == "" {
			t.Error("access_key_id did not parse")
		}
		if b.Schedule != "daily" {
			t.Errorf("schedule = %q, want daily", b.Schedule)
		}
		if b.Hour != 3 {
			t.Errorf("hour = %d, want 3", b.Hour)
		}
	})
}
