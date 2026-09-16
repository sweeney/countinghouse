package httpapi

import "testing"

// errorClass runs over strings that EMBED UP TO 2 KB OF UPSTREAM RESPONSE BODY.
// Matching bare digits anywhere in that let the remote end choose our
// classification. It could never leak — every branch returns a fixed string — but
// a health endpoint reporting the wrong cause is the failure this repo is careful
// about everywhere else.
func TestErrorClassReadsTheStatusCodeNotTheBody(t *testing.T) {
	const prefix = "octopus: "
	for _, tc := range []struct{ name, err, want string }{
		// The real shape: "octopus: %s (%d) for %s: %s".
		{"rate limited", prefix + `Too Many Requests (429) for https://h/v1/x: {"detail":"throttled"}`, "upstream rate limited"},
		{"forbidden", prefix + `Forbidden (403) for https://h/v1/x: {"detail":"no"}`, "upstream rejected our request"},
		{"server error", prefix + `Internal Server Error (500) for https://h/v1/x: oops`, "upstream unavailable"},
		{"bad gateway", prefix + `Bad Gateway (502) for https://h/v1/x: `, "upstream unavailable"},
		{"other http code", prefix + `Not Found (404) for https://h/v1/x: x`, "upstream error"},

		// The BODY must not decide the class.
		{"body says 429", prefix + `Not Found (404) for https://h/v1/x: {"count":4290,"note":"429 things"}`, "upstream error"},
		{"body says 403", prefix + `Internal Server Error (500) for https://h/v1/x: {"id":"40399"}`, "upstream unavailable"},
		{"body says 500", prefix + `Too Many Requests (429) for https://h/v1/x: {"retry_ms":500}`, "upstream rate limited"},

		// Non-HTTP failures still classify.
		{"archive", "collector: read archive for completeness: prices: range query: disk I/O error", "archive error"},
		{"timeout", prefix + `Get "https://h/v1/x": context deadline exceeded`, "upstream timeout"},

		// A three-digit number in a NON-octopus error must not be read as an HTTP
		// status. Both of these classified as upstream failures when the matcher
		// scanned the whole string, which is how a backup permissions problem came
		// to be reported as the supplier rejecting us.
		{"backup error containing 403", "r2: PutObject 403 AccessDenied for key x", "error"},
		{"dial error containing a port", "collector: dial tcp 10.0.0.1:8086: connection refused", "error"},

		{"empty", "", ""},
		{"unrecognised", "something else entirely", "error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := errorClass(tc.err); got != tc.want {
				t.Errorf("errorClass(%.70q)\n got %q\nwant %q", tc.err, got, tc.want)
			}
		})
	}
}

// Whatever it returns must be one of a fixed, known set. That is the property
// which makes this safe to publish unauthenticated, and it must hold for any
// input at all.
func TestErrorClassNeverEchoesItsInput(t *testing.T) {
	allowed := map[string]bool{
		"": true, "error": true, "archive error": true, "upstream timeout": true,
		"upstream rate limited": true, "upstream rejected our request": true,
		"upstream unavailable": true, "upstream error": true,
	}
	for _, in := range []string{
		"octopus: x (500) for https://h/p: SECRET-TRACE-abc123 10.1.2.3:5432",
		"prices: open archive at /var/lib/countinghouse/prices.db: denied",
		"r2: PutObject 403 for key production/backups/countinghouse/x.sqlite3",
		"\x00\xff binary garbage",
		"octopus: (999) for x: y",
	} {
		if got := errorClass(in); !allowed[got] {
			t.Errorf("errorClass returned %q, which is not in the fixed set", got)
		}
	}
}
