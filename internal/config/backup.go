package config

import (
	"fmt"
	"strings"
)

// Environment names, which double as the R2 key prefix.
//
// These two literals are not a local convention — `common/backup` writes keys as
// `{env}/backups/{service}/…` and the restore tooling matches on exactly these
// strings. A third value would produce backups that exist in the bucket and are
// invisible to every restore, so the set is closed.
const (
	EnvProduction  = "production"
	EnvDevelopment = "development"
)

// BackupServiceName is the key segment and filename prefix for this service's
// backups: `production/backups/countinghouse/2026/09/12/countinghouse-….sqlite3`.
//
// Deliberately a constant rather than config. It is an identity, not a preference,
// and making it settable invites a rename that strands every prior backup under a
// prefix nothing looks in any more.
const BackupServiceName = "countinghouse"

// DefaultBackupHour is the UTC hour scheduled backups run at. Early enough to be
// quiet, and stated here rather than left to the library because the library cannot
// tell an unset hour from a deliberate midnight.
const DefaultBackupHour = 3

// BackupConfig describes where the price archive is backed up to.
//
// The archive is the one thing countinghouse writes, and the entire reason to keep
// it is the day it stops being re-fetchable from the supplier. So a backup that is
// quietly not happening is worse than no backup: the operator believes the archive
// is safe and learns otherwise at the only moment it matters. Every PARTIAL
// configuration here is therefore refused at startup — see Validate. The one state
// that runs silently is no configuration at all, which is an explicit choice.
type BackupConfig struct {
	// Env is the R2 key prefix: "production" or "development". Required when
	// anything else here is set, and deliberately not defaulted — see Validate.
	Env string `yaml:"env"`

	// Bucket is the R2 bucket name, e.g. "countinghouse-sqlite". One bucket per
	// service, so the R2 API token can be scoped to it: a leaked countinghouse
	// credential then cannot read or overwrite another service's backups.
	Bucket string `yaml:"bucket"`

	// R2 credentials. AccountID also determines the endpoint host.
	AccountID   string `yaml:"account_id"`
	AccessKeyID string `yaml:"access_key_id"`

	// SecretAccessKey may be given inline or, preferably, in a file named by
	// SecretAccessKeyFile — the same pattern as influx.token/token_file, and read
	// by Load when the inline value is empty. A file keeps the secret out of a
	// config document that is otherwise harmless to copy around.
	SecretAccessKey     string `yaml:"secret_access_key"`
	SecretAccessKeyFile string `yaml:"secret_access_key_file"`

	// Schedule is "daily" (default), "weekly" (Sundays), "monthly" (1st) or "off".
	// "off" still allows on-demand backups; it only stops the timer.
	Schedule string `yaml:"schedule"`

	// Hour is the UTC hour 0–23 scheduled backups run at. Zero means MIDNIGHT, not
	// "unset", which is why Default() fills it in rather than leaving it to be
	// inferred.
	Hour int `yaml:"hour"`
}

// Configured reports whether the operator has asked for backups at all.
//
// True if ANY field is set, not all of them — a half-filled block is a configuration
// that was meant to work, and treating it as "off" is precisely how it would come to
// be silently ignored. Validate is what turns that into a refusal.
func (b BackupConfig) Configured() bool {
	return b.Env != "" || b.Bucket != "" || b.AccountID != "" ||
		b.AccessKeyID != "" || b.SecretAccessKey != "" || b.SecretAccessKeyFile != ""
}

// Validate checks the block against the archive path it is meant to protect.
//
// An empty block is legal and means backups are off. Anything else must be
// COMPLETE: every missing piece below describes a service that starts cleanly, logs
// nothing alarming, and never uploads.
//
// Errors name the offending key and never quote the secret — a config error is
// logged at startup, which is the least private place in a deployment.
func (b BackupConfig) Validate(dbPath string) error {
	if !b.Configured() {
		return nil
	}

	if dbPath == "" {
		return fmt.Errorf("prices.backup is configured but prices.db_path is not: " +
			"there is no archive to back up, so the backups would never contain anything")
	}

	var missing []string
	if b.Bucket == "" {
		missing = append(missing, "bucket")
	}
	if b.AccountID == "" {
		missing = append(missing, "account_id")
	}
	if b.AccessKeyID == "" {
		missing = append(missing, "access_key_id")
	}
	if b.SecretAccessKey == "" && b.SecretAccessKeyFile == "" {
		missing = append(missing, "secret_access_key (or secret_access_key_file)")
	}
	if len(missing) > 0 {
		return fmt.Errorf("prices.backup is incomplete, missing: %s; "+
			"refusing to start rather than run without the backups you configured",
			strings.Join(missing, ", "))
	}

	// Env last among the required keys so a wholly empty block reports the keys it
	// needs before quibbling about one value.
	switch b.Env {
	case EnvProduction, EnvDevelopment:
	case "":
		return fmt.Errorf("prices.backup.env is required: it is the R2 key prefix, and "+
			"leaving it unset silently files backups under %q — where a %q restore "+
			"cannot see them. Set it explicitly to %q or %q",
			EnvDevelopment, EnvProduction, EnvProduction, EnvDevelopment)
	default:
		return fmt.Errorf("prices.backup.env is %q, which must be %q or %q: it is the R2 "+
			"key prefix and the restore tooling matches those literals, so any other "+
			"value writes backups no restore looks for",
			b.Env, EnvProduction, EnvDevelopment)
	}

	switch b.Schedule {
	case "", "daily", "weekly", "monthly", "off":
	default:
		return fmt.Errorf("prices.backup.schedule is %q, which must be one of "+
			"daily, weekly, monthly, off (empty means daily): an unrecognised value is "+
			"silently treated as daily, so you would get neither what you asked for "+
			"nor a complaint", b.Schedule)
	}

	if b.Hour < 0 || b.Hour > 23 {
		return fmt.Errorf("prices.backup.hour is %d, which must be 0-23 (UTC): out of "+
			"range it is silently clamped to %d, so the backup runs at a time nobody chose",
			b.Hour, DefaultBackupHour)
	}

	return nil
}
