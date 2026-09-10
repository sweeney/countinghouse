package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sweeney/countinghouse/internal/testutil"
)

// ---------------------------------------------------------------------------
// Proactive alerting.
//
// /healthz is the right home for anything that self-heals: a fetch that failed
// and will be retried, a namespace serving its last-known snapshot. It is the
// WRONG home for the handful of conditions that silently corrupt money, because
// nothing reads /healthz at 02:00 — and by the time somebody does, a month of
// bills have been issued against a tariff that was wrong.
//
// So this package carries the small set of events worth pushing, and keeps the
// transport pluggable: the policy of WHAT deserves an alert belongs to
// countinghouse, the choice of HOW it is delivered does not.
// ---------------------------------------------------------------------------

func event(kind, summary string) Event {
	return Event{Kind: kind, Severity: SeverityWarn, Summary: summary}
}

// ---------------------------------------------------------------------------
// The slog notifier — always on, never fails
// ---------------------------------------------------------------------------

// The log is the floor: whatever else is configured, an alert must always land
// somewhere durable. A notifier that could fail to record anything would make
// every other guarantee here conditional.
func TestSlogNotifierAlwaysRecords(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	n := NewSlogNotifier(log)

	err := n.Notify(context.Background(), Event{
		Kind:     KindAgreementDrift,
		Severity: SeverityError,
		Summary:  "configured tariff disagrees with the supplier",
		Detail:   map[string]any{"configured": "A", "supplier": "B"},
	})
	if err != nil {
		t.Fatalf("the slog notifier must not fail: %v", err)
	}

	out := buf.String()
	for _, want := range []string{KindAgreementDrift, "configured", "supplier"} {
		if !strings.Contains(out, want) {
			t.Errorf("log output missing %q:\n%s", want, out)
		}
	}
	// Severity must reach the log LEVEL, not just the message body, or log-based
	// alerting cannot filter on it.
	if !strings.Contains(out, `"level":"ERROR"`) {
		t.Errorf("an error-severity event should log at ERROR:\n%s", out)
	}
}

func TestSlogNotifierMapsSeverityToLevel(t *testing.T) {
	for _, tc := range []struct {
		severity Severity
		wantLvl  string
	}{
		{severity: SeverityWarn, wantLvl: `"level":"WARN"`},
		{severity: SeverityError, wantLvl: `"level":"ERROR"`},
	} {
		var buf bytes.Buffer
		n := NewSlogNotifier(slog.New(slog.NewJSONHandler(&buf, nil)))
		if err := n.Notify(context.Background(), Event{Kind: "k", Severity: tc.severity, Summary: "s"}); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(buf.String(), tc.wantLvl) {
			t.Errorf("severity %q should log %s, got:\n%s", tc.severity, tc.wantLvl, buf.String())
		}
	}
}

// ---------------------------------------------------------------------------
// The webhook notifier
// ---------------------------------------------------------------------------

// A webhook keeps the transport out of this service: point it at ntfy, a Home
// Assistant hook, an MQTT bridge, whatever — without countinghouse knowing which.
func TestWebhookNotifierPostsEvent(t *testing.T) {
	type received struct {
		method, ctype string
		body          []byte
	}
	var (
		mu  sync.Mutex
		got []received
	)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, r.ContentLength)
		r.Body.Read(body) //nolint:errcheck
		mu.Lock()
		got = append(got, received{r.Method, r.Header.Get("Content-Type"), body})
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer ts.Close()

	clock := testutil.NewFakeClock(time.Date(2026, 9, 10, 20, 12, 24, 0, time.UTC))
	n := NewWebhookNotifier(ts.URL, WebhookOptions{Clock: clock})

	ev := Event{
		Kind:     KindPricesMissing,
		Severity: SeverityError,
		Summary:  "no prices for tomorrow",
		Detail:   map[string]any{"known_through": "2026-09-10T22:00:00Z"},
	}
	if err := n.Notify(context.Background(), ev); err != nil {
		t.Fatalf("Notify: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("got %d requests, want 1", len(got))
	}
	if got[0].method != http.MethodPost {
		t.Errorf("method = %s, want POST", got[0].method)
	}
	if !strings.HasPrefix(got[0].ctype, "application/json") {
		t.Errorf("content type = %q", got[0].ctype)
	}

	var payload map[string]any
	if err := json.Unmarshal(got[0].body, &payload); err != nil {
		t.Fatalf("body is not JSON: %v\n%s", err, got[0].body)
	}
	for _, field := range []string{"kind", "severity", "summary", "at", "service"} {
		if _, ok := payload[field]; !ok {
			t.Errorf("payload missing %q: %v", field, payload)
		}
	}
	// The timestamp comes from the injected clock, never time.Now: a consumer
	// correlating this against a bill needs the service's notion of now.
	if payload["at"] != "2026-09-10T20:12:24Z" {
		t.Errorf("at = %v, want the injected clock's instant", payload["at"])
	}
	if payload["service"] != "countinghouse" {
		t.Errorf("service = %v, want countinghouse so a shared endpoint can tell who called", payload["service"])
	}
}

// A notification endpoint being down must never break the thing that was trying
// to report. The error is returned so a caller can count it, but the caller's
// own work is expected to continue — see the collector, which treats this as
// fail-open.
func TestWebhookNotifierFailureModes(t *testing.T) {
	for _, tc := range []struct {
		name    string
		handler http.HandlerFunc
		closed  bool
	}{
		{
			name:    "non-2xx is an error",
			handler: func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) },
		},
		{
			name:    "404 is an error",
			handler: func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) },
		},
		{
			name:    "401 is an error",
			handler: func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) },
		},
		{
			name:   "endpoint down is an error",
			closed: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := tc.handler
			if h == nil {
				h = func(http.ResponseWriter, *http.Request) {}
			}
			ts := httptest.NewServer(h)
			url := ts.URL
			if tc.closed {
				ts.Close()
			} else {
				defer ts.Close()
			}

			n := NewWebhookNotifier(url, WebhookOptions{})
			err := n.Notify(context.Background(), event("k", "s"))
			if err == nil {
				t.Error("want an error so the caller can count the failure")
			}
		})
	}
}

// A hanging endpoint must not hold the caller. Without a bounded timeout a
// notification attempt could outlive the collector tick that produced it.
func TestWebhookNotifierTimesOut(t *testing.T) {
	block := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-block:
		case <-r.Context().Done():
		}
	}))
	defer func() { close(block); ts.Close() }()

	n := NewWebhookNotifier(ts.URL, WebhookOptions{Timeout: 50 * time.Millisecond})
	start := time.Now()
	err := n.Notify(context.Background(), event("k", "s"))
	if err == nil {
		t.Fatal("want a timeout error")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("took %v; the timeout did not bound the attempt", elapsed)
	}
}

func TestWebhookNotifierRespectsContext(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer ts.Close()

	n := NewWebhookNotifier(ts.URL, WebhookOptions{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := n.Notify(ctx, event("k", "s"))
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want it to wrap context.Canceled", err)
	}
}

// ---------------------------------------------------------------------------
// Fan-out
// ---------------------------------------------------------------------------

// One failing transport must not stop the others. The log notifier is usually
// one of them, so this is what guarantees an alert is still recorded when the
// webhook is down.
func TestMultiContinuesAfterAFailure(t *testing.T) {
	failing := &fakeNotifier{err: errors.New("down")}
	working := &fakeNotifier{}

	m := Multi(failing, working)
	err := m.Notify(context.Background(), event("k", "s"))
	if err == nil {
		t.Error("Multi should report that one transport failed")
	}
	if len(working.events) != 1 {
		t.Errorf("the working notifier got %d events, want 1 — a failure must not short-circuit", len(working.events))
	}
}

func TestMultiWithNoNotifiersIsANoOp(t *testing.T) {
	if err := Multi().Notify(context.Background(), event("k", "s")); err != nil {
		t.Errorf("an empty Multi should be a no-op, got %v", err)
	}
}

// A nil notifier must be usable, so call sites never need a nil check before
// reporting something.
func TestNopNotifier(t *testing.T) {
	var n Notifier = Nop{}
	if err := n.Notify(context.Background(), event("k", "s")); err != nil {
		t.Errorf("Nop should never fail: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Throttling — alert on TRANSITION, not on state
// ---------------------------------------------------------------------------

// This is what makes proactive alerting tolerable. The publication watch runs
// every five minutes, so a condition like "tomorrow's prices have not arrived"
// is true on many consecutive ticks. Alerting each time would deliver dozens of
// identical messages in an evening, and the reliable outcome of that is a muted
// channel — which is worse than no alerting at all, because it fails silently.
func TestThrottleSuppressesRepeatsOfTheSameCondition(t *testing.T) {
	inner := &fakeNotifier{}
	clock := testutil.NewFakeClock(time.Date(2026, 9, 10, 16, 0, 0, 0, time.UTC))
	th := NewThrottle(inner, time.Hour, clock)

	ev := event(KindPricesMissing, "no prices for tomorrow")

	// First occurrence gets through.
	if err := th.Notify(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	// The same condition five minutes later does not.
	clock.Advance(5 * time.Minute)
	if err := th.Notify(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	clock.Advance(5 * time.Minute)
	if err := th.Notify(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	if len(inner.events) != 1 {
		t.Fatalf("delivered %d events, want 1 — repeats within the cooldown must be suppressed", len(inner.events))
	}

	// Past the cooldown it re-alerts, because a condition that is STILL true an
	// hour later is worth saying again.
	clock.Advance(time.Hour)
	if err := th.Notify(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	if len(inner.events) != 2 {
		t.Errorf("delivered %d events, want 2 after the cooldown elapsed", len(inner.events))
	}
}

// Different conditions are throttled independently: a price gap must not mask a
// restatement just because they happened in the same minute.
func TestThrottleKeysOnKindAndDetail(t *testing.T) {
	inner := &fakeNotifier{}
	clock := testutil.NewFakeClock(time.Date(2026, 9, 10, 16, 0, 0, 0, time.UTC))
	th := NewThrottle(inner, time.Hour, clock)
	ctx := context.Background()

	if err := th.Notify(ctx, event(KindPricesMissing, "no prices")); err != nil {
		t.Fatal(err)
	}
	if err := th.Notify(ctx, event(KindRestatement, "a price changed")); err != nil {
		t.Fatal(err)
	}
	if err := th.Notify(ctx, event(KindAgreementDrift, "tariff mismatch")); err != nil {
		t.Fatal(err)
	}
	if len(inner.events) != 3 {
		t.Errorf("delivered %d, want 3 — distinct kinds throttle independently", len(inner.events))
	}

	// Same kind but a different DedupKey is also a distinct condition: two
	// different slots being restated are two different facts.
	inner.events = nil
	a := Event{Kind: KindRestatement, Severity: SeverityWarn, Summary: "x", DedupKey: "slot-1"}
	b := Event{Kind: KindRestatement, Severity: SeverityWarn, Summary: "x", DedupKey: "slot-2"}
	if err := th.Notify(ctx, a); err != nil {
		t.Fatal(err)
	}
	if err := th.Notify(ctx, b); err != nil {
		t.Fatal(err)
	}
	if err := th.Notify(ctx, a); err != nil { // repeat of the first
		t.Fatal(err)
	}
	if len(inner.events) != 2 {
		t.Errorf("delivered %d, want 2 — distinct dedup keys are distinct conditions", len(inner.events))
	}
}

// A condition that clears and then comes back is NEWS, even inside the cooldown.
// Resolve() is how the caller says "this is no longer true", and without it a
// flapping problem would be invisible after its first report.
func TestThrottleResolveAllowsImmediateReAlert(t *testing.T) {
	inner := &fakeNotifier{}
	clock := testutil.NewFakeClock(time.Date(2026, 9, 10, 16, 0, 0, 0, time.UTC))
	th := NewThrottle(inner, time.Hour, clock)
	ctx := context.Background()

	ev := event(KindPricesMissing, "no prices for tomorrow")
	if err := th.Notify(ctx, ev); err != nil {
		t.Fatal(err)
	}

	// The prices arrive: the condition is resolved.
	th.Resolve(ev.Kind, ev.DedupKey)

	// It breaks again a minute later. That is a new fact and must be delivered
	// even though the cooldown has not elapsed.
	clock.Advance(time.Minute)
	if err := th.Notify(ctx, ev); err != nil {
		t.Fatal(err)
	}
	if len(inner.events) != 2 {
		t.Errorf("delivered %d events, want 2 — a recurrence after resolution is news", len(inner.events))
	}
}

// Resolving something that was never firing must be harmless: callers resolve
// unconditionally on every successful tick rather than tracking state themselves.
func TestThrottleResolveUnknownIsHarmless(t *testing.T) {
	inner := &fakeNotifier{}
	clock := testutil.NewFakeClock(time.Now())
	th := NewThrottle(inner, time.Hour, clock)

	th.Resolve("never-fired", "")
	if len(inner.events) != 0 {
		t.Error("Resolve must not itself deliver anything")
	}
}

// A zero cooldown disables throttling, which is what tests of downstream
// behaviour want.
func TestThrottleZeroCooldownDeliversEverything(t *testing.T) {
	inner := &fakeNotifier{}
	clock := testutil.NewFakeClock(time.Now())
	th := NewThrottle(inner, 0, clock)
	ctx := context.Background()

	ev := event(KindPricesMissing, "s")
	for i := 0; i < 3; i++ {
		if err := th.Notify(ctx, ev); err != nil {
			t.Fatal(err)
		}
	}
	if len(inner.events) != 3 {
		t.Errorf("delivered %d, want 3 with throttling disabled", len(inner.events))
	}
}

// The throttle is shared between the collector's goroutines, so it must be safe
// for concurrent use — and must still suppress correctly under contention
// rather than letting a race leak duplicates.
func TestThrottleIsConcurrencySafe(t *testing.T) {
	inner := &fakeNotifier{}
	clock := testutil.NewFakeClock(time.Date(2026, 9, 10, 16, 0, 0, 0, time.UTC))
	th := NewThrottle(inner, time.Hour, clock)
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			th.Notify(ctx, event(KindPricesMissing, "s")) //nolint:errcheck
		}()
	}
	wg.Wait()

	if n := len(inner.events); n != 1 {
		t.Errorf("delivered %d events from 16 concurrent identical reports, want exactly 1", n)
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// fakeNotifier records what it was asked to send.
type fakeNotifier struct {
	mu     sync.Mutex
	events []Event
	err    error
}

func (f *fakeNotifier) Notify(_ context.Context, e Event) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, e)
	return f.err
}
