package octopus

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Failure modes.
//
// The collector is fail-open by design: a fetch that does not work must leave
// the archive untouched and degrade health, never write a guess. That policy is
// only trustworthy if the client reports failure precisely — which status it
// got, whether retrying could help, and how long to wait. These tests pin each
// failure mode we can actually expect from a public API behind a CDN.
//
// Nothing here sleeps. Backoff goes through an injected sleeper so a retry test
// costs microseconds; a suite that took real seconds per retry would stop being
// run, which is the real failure.
// ---------------------------------------------------------------------------

// The service being unreachable is the most common failure in practice: a
// restart, DNS, a dropped link. It must surface as an error, not an empty
// result set that a caller could mistake for "no prices published".
func TestServiceDownReturnsError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := ts.URL
	ts.Close() // nothing is listening now

	c, err := New(Options{BaseURL: url, Sleep: newFakeSleeper().Sleep})
	if err != nil {
		t.Fatal(err)
	}

	rates, err := c.UnitRates(context.Background(), agileN(t), time.Time{}, time.Time{})
	if err == nil {
		t.Fatalf("expected an error with nothing listening; got %d rates", len(rates))
	}
	if rates != nil {
		t.Errorf("rates = %v, want nil on error — a partial result is worse than none", rates)
	}
	// A transport error is worth retrying; the caller needs to be told that.
	if !IsRetryable(err) {
		t.Errorf("a connection failure should be retryable, got %v", err)
	}
}

// Status-code handling, in one table. The distinction that matters is
// retryable-or-not: retrying a 403 forever would hammer the API and never
// succeed, while giving up on a 503 would drop a day of prices we could have had.
func TestStatusCodeHandling(t *testing.T) {
	for _, tc := range []struct {
		name          string
		status        int
		body          string
		wantRetryable bool
		// wantAttempts is how many HTTP requests the client should make in
		// total, given MaxRetries=2 (so up to 3 attempts).
		wantAttempts int
	}{
		{
			// 403 is what a bad or revoked API key looks like on the account
			// endpoints. Retrying cannot fix a credential.
			name: "403 forbidden is terminal", status: http.StatusForbidden,
			body:          `{"detail":"You do not have permission to perform this action."}`,
			wantRetryable: false, wantAttempts: 1,
		},
		{
			// A 404 means we built a path for a tariff that does not exist —
			// a bug or a bad config, not a transient condition.
			name: "404 not found is terminal", status: http.StatusNotFound,
			body:          `{"detail":"Not found."}`,
			wantRetryable: false, wantAttempts: 1,
		},
		{
			name: "401 unauthorized is terminal", status: http.StatusUnauthorized,
			body:          `{"detail":"Invalid token."}`,
			wantRetryable: false, wantAttempts: 1,
		},
		{
			name: "400 bad request is terminal", status: http.StatusBadRequest,
			body:          `{"detail":"Invalid period_from."}`,
			wantRetryable: false, wantAttempts: 1,
		},
		{
			// Rate limiting is explicitly temporary.
			name: "429 too many requests is retried", status: http.StatusTooManyRequests,
			body:          `{"detail":"Request was throttled."}`,
			wantRetryable: true, wantAttempts: 3,
		},
		{
			name: "500 internal error is retried", status: http.StatusInternalServerError,
			body:          `{"detail":"Server Error"}`,
			wantRetryable: true, wantAttempts: 3,
		},
		{
			name: "502 bad gateway is retried", status: http.StatusBadGateway,
			body:          "<html><body>502 Bad Gateway</body></html>",
			wantRetryable: true, wantAttempts: 3,
		},
		{
			// A CDN shedding load, or Octopus in maintenance.
			name: "503 unavailable is retried", status: http.StatusServiceUnavailable,
			body:          `{"detail":"Service temporarily unavailable."}`,
			wantRetryable: true, wantAttempts: 3,
		},
		{
			name: "504 gateway timeout is retried", status: http.StatusGatewayTimeout,
			body:          "upstream timed out",
			wantRetryable: true, wantAttempts: 3,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := newTestServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
				w.WriteHeader(tc.status)
				w.Write([]byte(tc.body)) //nolint:errcheck
			})
			sleeper := newFakeSleeper()
			c, err := New(Options{BaseURL: ts.URL, Sleep: sleeper.Sleep, MaxRetries: 2})
			if err != nil {
				t.Fatal(err)
			}

			_, err = c.UnitRates(context.Background(), agileN(t), time.Time{}, time.Time{})
			if err == nil {
				t.Fatal("expected an error")
			}

			var apiErr *APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("error %v is not an *APIError; callers cannot inspect the status", err)
			}
			if apiErr.StatusCode != tc.status {
				t.Errorf("StatusCode = %d, want %d", apiErr.StatusCode, tc.status)
			}
			if IsRetryable(err) != tc.wantRetryable {
				t.Errorf("IsRetryable = %v, want %v", IsRetryable(err), tc.wantRetryable)
			}
			if n := len(ts.got()); n != tc.wantAttempts {
				t.Errorf("made %d attempts, want %d", n, tc.wantAttempts)
			}
			// The body is worth keeping: it is what tells a human which of
			// several possible causes it was. But it must be bounded, because
			// an error page can be megabytes.
			if tc.body != "" && apiErr.Body == "" {
				t.Error("APIError.Body is empty; the response body is the only diagnostic we get")
			}
		})
	}
}

// A 429 carrying Retry-After must be obeyed, not overridden by our own backoff
// curve. Ignoring it is how a client earns a longer ban.
func TestRetryAfterSecondsIsHonoured(t *testing.T) {
	body := fixture(t, "unit_rates_simple.json")
	ts := newTestServer(t, func(w http.ResponseWriter, _ *http.Request, attempt int) {
		if attempt == 0 {
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"detail":"Request was throttled."}`)) //nolint:errcheck
			return
		}
		w.Write(body) //nolint:errcheck
	})
	sleeper := newFakeSleeper()
	c, err := New(Options{BaseURL: ts.URL, Sleep: sleeper.Sleep, MaxRetries: 3})
	if err != nil {
		t.Fatal(err)
	}

	rates, err := c.UnitRates(context.Background(), agileN(t), time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("should have succeeded on the retry: %v", err)
	}
	if len(rates) == 0 {
		t.Fatal("no rates after a successful retry")
	}

	slept := sleeper.durations()
	if len(slept) != 1 {
		t.Fatalf("slept %d times, want 1", len(slept))
	}
	if slept[0] != 7*time.Second {
		t.Errorf("slept %v, want the 7s the server asked for", slept[0])
	}
}

// Retry-After may also be an HTTP date. Both forms appear in the wild.
func TestRetryAfterHTTPDateIsHonoured(t *testing.T) {
	now := time.Date(2026, 9, 10, 20, 0, 0, 0, time.UTC)
	body := fixture(t, "unit_rates_simple.json")
	ts := newTestServer(t, func(w http.ResponseWriter, _ *http.Request, attempt int) {
		if attempt == 0 {
			w.Header().Set("Retry-After", now.Add(12*time.Second).Format(http.TimeFormat))
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Write(body) //nolint:errcheck
	})
	sleeper := newFakeSleeper()
	c, err := New(Options{
		BaseURL: ts.URL, Sleep: sleeper.Sleep, MaxRetries: 3,
		Clock: fixedClock{now},
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := c.UnitRates(context.Background(), agileN(t), time.Time{}, time.Time{}); err != nil {
		t.Fatalf("should have succeeded on the retry: %v", err)
	}
	slept := sleeper.durations()
	if len(slept) != 1 {
		t.Fatalf("slept %d times, want 1", len(slept))
	}
	if slept[0] != 12*time.Second {
		t.Errorf("slept %v, want 12s derived from the HTTP-date Retry-After", slept[0])
	}
}

// An absurd or hostile Retry-After must not park the collector for a day. It is
// clamped, and the clamp is visible rather than silent.
func TestRetryAfterIsClamped(t *testing.T) {
	ts := newTestServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.Header().Set("Retry-After", "86400") // a full day
		w.WriteHeader(http.StatusTooManyRequests)
	})
	sleeper := newFakeSleeper()
	c, err := New(Options{BaseURL: ts.URL, Sleep: sleeper.Sleep, MaxRetries: 1})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := c.UnitRates(context.Background(), agileN(t), time.Time{}, time.Time{}); err == nil {
		t.Fatal("expected an error")
	}
	for _, d := range sleeper.durations() {
		if d > maxRetryAfter {
			t.Errorf("slept %v, which exceeds the %v clamp", d, maxRetryAfter)
		}
	}
}

// Without Retry-After, backoff must still grow rather than hammering at a fixed
// interval — and must stay bounded.
func TestBackoffGrowsAndIsBounded(t *testing.T) {
	ts := newTestServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	sleeper := newFakeSleeper()
	c, err := New(Options{BaseURL: ts.URL, Sleep: sleeper.Sleep, MaxRetries: 4})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := c.UnitRates(context.Background(), agileN(t), time.Time{}, time.Time{}); err == nil {
		t.Fatal("expected an error after exhausting retries")
	}

	slept := sleeper.durations()
	if len(slept) != 4 {
		t.Fatalf("slept %d times, want 4 (one per retry)", len(slept))
	}
	for i := 1; i < len(slept); i++ {
		if slept[i] < slept[i-1] {
			t.Errorf("backoff went backwards: %v then %v", slept[i-1], slept[i])
		}
	}
	for _, d := range slept {
		if d > maxRetryAfter {
			t.Errorf("backoff %v exceeds the %v bound", d, maxRetryAfter)
		}
	}
	t.Logf("backoff curve: %v", slept)
}

// Retries are finite. Exhausting them reports the LAST failure, so the logs name
// what actually kept happening.
func TestRetriesAreExhaustedAndReportTheLastFailure(t *testing.T) {
	ts := newTestServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(`{"detail":"still down"}`)) //nolint:errcheck
	})
	c, err := New(Options{BaseURL: ts.URL, Sleep: newFakeSleeper().Sleep, MaxRetries: 2})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.UnitRates(context.Background(), agileN(t), time.Time{}, time.Time{})
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("want *APIError, got %v", err)
	}
	if apiErr.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", apiErr.StatusCode)
	}
	if !strings.Contains(apiErr.Body, "still down") {
		t.Errorf("body %q should carry the server's explanation", apiErr.Body)
	}
	if n := len(ts.got()); n != 3 {
		t.Errorf("made %d attempts, want 3 (initial + 2 retries)", n)
	}
}

// ---------------------------------------------------------------------------
// Malformed responses
// ---------------------------------------------------------------------------

// A 200 with a body we cannot parse is a real case: a captive portal, a proxy
// error page, a truncated response. It must be an error, never a silent zero
// result — "no prices" and "could not read prices" lead to opposite actions.
func TestMalformedResponses(t *testing.T) {
	for _, tc := range []struct {
		name, body, wantIn string
	}{
		{name: "not json at all", body: `<html><title>504 Gateway Timeout</title></html>`, wantIn: "decode"},
		{name: "truncated json", body: `{"count":2,"results":[{"value_exc_vat":10.0,`, wantIn: "decode"},
		{name: "empty body", body: ``, wantIn: "decode"},
		{name: "json null", body: `null`, wantIn: ""},
		{name: "results is not an array", body: `{"count":1,"results":{"oops":true}}`, wantIn: "decode"},
		{
			// A slot whose timestamp we cannot parse is not something to skip
			// quietly: a silently dropped slot becomes unpriced energy weeks
			// later, and by then nobody can tell whether the price was missing
			// or we ate it.
			name:   "unparseable valid_from",
			body:   `{"count":1,"next":null,"results":[{"value_exc_vat":10.0,"value_inc_vat":10.5,"valid_from":"yesterday","valid_to":null,"payment_method":null}]}`,
			wantIn: "valid_from",
		},
		{
			name:   "missing valid_from",
			body:   `{"count":1,"next":null,"results":[{"value_exc_vat":10.0,"value_inc_vat":10.5,"valid_to":null,"payment_method":null}]}`,
			wantIn: "valid_from",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := newTestServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(tc.body)) //nolint:errcheck
			})
			c := newClient(t, ts, nil)

			rates, err := c.UnitRates(context.Background(), agileN(t), time.Time{}, time.Time{})
			if err == nil {
				// `null` decodes to a zero struct with no results. That is
				// indistinguishable from an empty page, so it is allowed to be
				// a non-error — but it must yield nothing, not a phantom slot.
				if len(rates) != 0 {
					t.Fatalf("parsed %d rates from %q", len(rates), tc.body)
				}
				if tc.wantIn != "" {
					t.Fatalf("expected an error mentioning %q", tc.wantIn)
				}
				return
			}
			if tc.wantIn != "" && !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("error %q should mention %q", err, tc.wantIn)
			}
			if rates != nil {
				t.Errorf("rates should be nil on error, got %d", len(rates))
			}
		})
	}
}

// A huge error page must not be held in memory in full, nor logged in full.
func TestOversizedBodyIsBounded(t *testing.T) {
	huge := strings.Repeat("x", 5<<20) // 5 MiB
	ts := newTestServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(huge)) //nolint:errcheck
	})
	c, err := New(Options{BaseURL: ts.URL, Sleep: newFakeSleeper().Sleep, MaxRetries: 0})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.UnitRates(context.Background(), agileN(t), time.Time{}, time.Time{})
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("want *APIError, got %v", err)
	}
	if len(apiErr.Body) > maxErrorBody {
		t.Errorf("retained %d bytes of error body, want at most %d", len(apiErr.Body), maxErrorBody)
	}
}

// ---------------------------------------------------------------------------
// Context
// ---------------------------------------------------------------------------

// A cancelled context must abandon the request promptly. On shutdown the
// collector's context is cancelled, and a client that ignored it would hold the
// process open past its graceful-shutdown budget.
func TestContextCancellationAbandonsRequest(t *testing.T) {
	release := make(chan struct{})
	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	t.Cleanup(func() { close(release) })

	c := newClient(t, ts, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled

	_, err := c.UnitRates(ctx, agileN(t), time.Time{}, time.Time{})
	if err == nil {
		t.Fatal("expected an error from a cancelled context")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error %v should wrap context.Canceled so shutdown is distinguishable from a fetch failure", err)
	}
	// A cancelled context is not the API's fault; it must not look retryable.
	if IsRetryable(err) {
		t.Error("a cancelled context should not be reported as retryable")
	}
}

// Cancellation DURING a backoff wait must also abandon, rather than finishing
// the sleep and firing another doomed request.
func TestContextCancellationDuringBackoff(t *testing.T) {
	ts := newTestServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	sleeper := newFakeSleeper()
	sleeper.failAt = 0 // the very first backoff returns context.Canceled
	c, err := New(Options{BaseURL: ts.URL, Sleep: sleeper.Sleep, MaxRetries: 5})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.UnitRates(context.Background(), agileN(t), time.Time{}, time.Time{})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error %v should wrap context.Canceled", err)
	}
	if n := len(ts.got()); n != 1 {
		t.Errorf("made %d requests, want 1 — the retry should have been abandoned", n)
	}
}

// fixedClock is a Clock stuck at one instant, for the HTTP-date Retry-After test.
type fixedClock struct{ t time.Time }

func (f fixedClock) Now() time.Time { return f.t }
