package httpapi

import "time"

// BackupHealth is the state of the price archive's offsite backup, as reported on
// /healthz and /metrics.
//
// An httpapi-local type rather than the backup library's own, for the same reason
// PriceHealth is: the HTTP layer should not import the backup machinery, and a test
// should be able to produce any state without standing up an uploader. main.go
// adapts between them.
//
// Nothing here names a credential, and nothing may be added that does. /healthz is
// built to be polled by anything, and the R2 secret is the most valuable thing in
// the process to leak. Bucket and env are locations, not secrets, and they are here
// because "backed up where?" is the first question after "backed up?".
type BackupHealth struct {
	Bucket   string `json:"bucket"`
	Env      string `json:"env"`
	Schedule string `json:"schedule,omitempty"`
	Hour     int    `json:"hour"`

	// LastAttempt moves on every run; LastSuccess only on one that worked. Equal
	// values mean healthy; a LastSuccess lagging behind means we are failing now.
	LastAttempt time.Time `json:"last_attempt,omitempty"`
	LastSuccess time.Time `json:"last_success,omitempty"`

	// LastKey is the object key of the most recent successful upload, so the
	// backup can be found without listing the bucket.
	LastKey   string `json:"last_key,omitempty"`
	LastError string `json:"last_error,omitempty"`

	Successes int `json:"successes"`
	Failures  int `json:"failures"`
}

// BackupProvider supplies the backup health behind /healthz and /metrics.
//
// Server.Backups may be nil, and normally IS in development and on any deployment
// with no archive to protect: the block is then omitted entirely rather than
// rendered empty, since a zeroed block would read as a broken backup rather than as
// no backup. The provider may also return nil for the same reason.
type BackupProvider interface {
	BackupHealth() *BackupHealth
}

// backupStaleAfter is how long a backup may go un-taken before the service reports
// degraded.
//
// Two days, against a daily schedule. One day would flag a single missed run, which
// a restart near the scheduled hour can cause legitimately; a week would let a
// wedged scheduler go unnoticed for most of it. Two days means something has missed
// twice, which is no longer a coincidence.
const backupStaleAfter = 48 * time.Hour

// backupVerdict reports whether backups are in good order, and why not when they
// are not.
//
// Degraded, never unavailable: no data route depends on last night's upload, so
// failing the service would be a lie. But a silent "ok" while the archive has never
// once been uploaded is the exact failure this exists to catch — an operator who
// believes the one non-rebuildable thing in the service is protected, and finds out
// otherwise at the only moment it matters.
//
// Three conditions, and the order matters:
//
//   - a failure since the last success. What is in the bucket no longer covers what
//     would be lost, and the archive has grown since.
//   - never succeeded at all, which is what a typo'd credential or a missing bucket
//     leaves behind. Reported only once something has been attempted: a service up
//     for a minute with its first run hours away has not missed anything, and
//     complaining would make every restart look like a fault.
//   - stale, with nothing to show for it. A wedged scheduler produces no error at
//     all, just silence, so a verdict keyed only on LastError would call it healthy.
//     Not applied when the schedule is "off", where the operator asked for on-demand
//     backups only and an old one is expected.
func backupVerdict(h *BackupHealth, now time.Time) (degraded bool, reason string) {
	if h == nil {
		return false, ""
	}
	if h.LastError != "" {
		return true, "price archive backup failing: " + h.LastError
	}
	if h.LastSuccess.IsZero() {
		if h.LastAttempt.IsZero() {
			return false, "" // configured, nothing asked of it yet
		}
		return true, "price archive has never been backed up successfully"
	}
	if h.Schedule != "off" && now.Sub(h.LastSuccess) > backupStaleAfter {
		return true, "price archive backup is stale"
	}
	return false, ""
}
