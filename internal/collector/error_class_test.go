package collector

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/sweeney/countinghouse/internal/octopus"
)

// The class must be derived where the TYPE still exists.
//
// Classifying from Status.LastError could not work: every call site wraps before
// storing — c.fail(fmt.Errorf("collector: horizon probe: %w", err)) — so the
// stored string always begins "collector: <stage>: ", and a classifier anchored
// to the octopus prefix matched none of it. Building the fixture from a real
// *octopus.APIError, wrapped the way Sync wraps it, is what makes this testable
// against the path rather than against a literal.
func TestFailRecordsTheClassFromTheTypedError(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"429", &octopus.APIError{StatusCode: 429, Status: "429 Too Many Requests",
			Body: `{"detail":"Request was throttled."}`, URL: "https://api.octopus.energy/v1/x"},
			"upstream rate limited"},
		{"503", &octopus.APIError{StatusCode: 503, Status: "503 Service Unavailable",
			Body: "", URL: "https://api.octopus.energy/v1/x"}, "upstream unavailable"},
		{"403", &octopus.APIError{StatusCode: 403, Status: "403 Forbidden",
			URL: "https://api.octopus.energy/v1/x"}, "upstream rejected our request"},
		{"404", &octopus.APIError{StatusCode: 404, Status: "404 Not Found",
			// A body full of misleading digits must not reach the decision.
			Body: `{"count":4290,"note":"500 things","id":"40399"}`,
			URL:  "https://api.octopus.energy/v1/x"}, "upstream error"},
		{"archive", errors.New("prices: open archive at /var/lib/countinghouse/prices.db: denied"),
			"archive error"},
		// A cancelled request carries the typed sentinel; a stalled socket only ever
		// reaches us as text. Both are the same thing to whoever is paged.
		{"deadline", context.DeadlineExceeded, "upstream timeout"},
		{"dial timeout", errors.New(
			"Get \"https://api.octopus.energy/v1/x\": dial tcp: i/o timeout"), "upstream timeout"},
		{"plain", errors.New("something else"), "error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Exactly how Sync stores it.
			wrapped := tc.err
			c := &Collector{}
			c.fail(wrapError(wrapped))
			if got := c.Status().LastErrorClass; got != tc.want {
				t.Errorf("LastErrorClass = %q, want %q\n  (stored: %q)",
					got, tc.want, c.Status().LastError)
			}
			// The full text is kept for /metrics.
			if c.Status().LastError == "" {
				t.Error("LastError is empty; the detail must survive for /metrics")
			}
		})
	}
}

// wrapError reproduces the wrapping every c.fail call site applies.
func wrapError(err error) error {
	return fmt.Errorf("collector: horizon probe: %w", err)
}

// The way back up.
//
// The class was added to Status and wired into fail(), but markSuccess cleared only
// LastError — and priceVerdict degrades on EITHER field, so the first transient 429
// or one bad disk moment left /healthz red until a restart. Exactly the failures
// this collector is built to shrug off, on the one endpoint built to be polled.
//
// This is the second time a health field has been correct on the way down and wrong
// on the way up, which is why the test asserts recovery rather than just reporting.
func TestMarkSuccessClearsTheClassAsWellAsTheText(t *testing.T) {
	c := &Collector{}
	c.fail(wrapError(&octopus.APIError{StatusCode: 429, Status: "429 Too Many Requests",
		URL: "https://api.octopus.energy/v1/x"}))
	if st := c.Status(); st.LastError == "" || st.LastErrorClass == "" {
		t.Fatalf("precondition: a failure must record both fields, got %q / %q",
			st.LastError, st.LastErrorClass)
	}

	c.markSuccess(time.Date(2026, 9, 17, 18, 0, 0, 0, time.UTC), SyncResult{})

	st := c.Status()
	if st.LastError != "" {
		t.Errorf("LastError survived a success: %q", st.LastError)
	}
	if st.LastErrorClass != "" {
		t.Errorf("LastErrorClass survived a success: %q — /healthz stays degraded forever", st.LastErrorClass)
	}
	// And the counter does NOT reset: the failure happened, and a rising failure
	// count against a clean current state is how a flapping upstream is spotted.
	if st.Failures != 1 {
		t.Errorf("Failures = %d, want 1 — recovery must not erase the history", st.Failures)
	}
}
