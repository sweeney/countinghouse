// Package notify pushes the small set of conditions that must not wait to be
// noticed.
//
// `/healthz` is the right home for anything that self-heals — a fetch that
// failed and will be retried, a namespace serving its last-known snapshot. It is
// the wrong home for the few conditions that silently corrupt money, because
// nothing reads /healthz at 02:00, and by the time somebody does, a month of
// bills have been issued against a tariff that was wrong.
//
// The split is deliberate: the POLICY of what deserves an alert belongs to
// countinghouse (see the Kind constants), while the TRANSPORT does not. A
// webhook keeps the choice of ntfy, Home Assistant, an MQTT bridge or anything
// else outside this service.
//
// Everything here is fail-open from the caller's point of view. Notify returns
// an error so a caller can count it, but no caller should abandon its own work
// because a notification did not land — failing to bill because the alerting
// endpoint was down would be a worse outcome than the thing being alerted about.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/sweeney/countinghouse/internal/testutil"
)

// Severity is how loud an event is. Deliberately only two values: something is
// either worth interrupting somebody for or it belongs on /healthz. A middle
// tier would just become a second place for things to be ignored.
type Severity string

const (
	SeverityWarn  Severity = "warn"
	SeverityError Severity = "error"
)

// Event kinds. These ARE the policy — the list of things judged unable to wait
// for somebody to read a health endpoint. Each one can make a bill wrong without
// anything else looking broken.
const (
	// KindAgreementDrift: the configured tariff disagrees with what the supplier
	// says the account is actually on. Every bill priced under the wrong
	// agreement is wrong, and nothing else in the system looks unhealthy.
	KindAgreementDrift = "agreement_drift"

	// KindPricesMissing: prices for a period we are about to need are not held.
	// Energy in an unpriced slot cannot be billed, and the gap is invisible
	// until somebody asks for that window.
	KindPricesMissing = "prices_missing"

	// KindRestatement: the supplier changed a price we had already stored, and
	// possibly already billed. The archive records it, but somebody has to know.
	KindRestatement = "restatement"

	// KindValidationRejected: slots were refused by the validation gates. One is
	// a curiosity; a sustained stream means we have misunderstood the feed.
	KindValidationRejected = "validation_rejected"

	// KindDayIncomplete: a day was published but is missing slots, and has
	// stayed that way past the point we stop waiting.
	KindDayIncomplete = "day_incomplete"
)

// Event is one thing worth telling somebody about.
type Event struct {
	Kind     string
	Severity Severity
	Summary  string

	// Detail carries the specifics a human needs to act without going digging.
	Detail map[string]any

	// DedupKey distinguishes two instances of the same Kind that are genuinely
	// different facts — two different slots restated, say. Events sharing a Kind
	// and DedupKey are treated as the same ongoing condition by Throttle.
	// Empty means the Kind alone identifies the condition.
	DedupKey string
}

// Notifier delivers events.
type Notifier interface {
	Notify(ctx context.Context, e Event) error
}

// Nop discards events. It exists so a call site never needs a nil check before
// reporting something.
type Nop struct{}

func (Nop) Notify(context.Context, Event) error { return nil }

// ---------------------------------------------------------------------------
// slog
// ---------------------------------------------------------------------------

// SlogNotifier writes events to the service log.
//
// This is the floor, and it is always configured: whatever else is in place, an
// alert must land somewhere durable. It cannot fail, which is what stops every
// other guarantee in this package from being conditional on a network.
type SlogNotifier struct{ log *slog.Logger }

// NewSlogNotifier returns a notifier writing to log, or to slog.Default() when
// log is nil.
func NewSlogNotifier(log *slog.Logger) *SlogNotifier {
	if log == nil {
		log = slog.Default()
	}
	return &SlogNotifier{log: log}
}

// Notify logs the event at the level matching its severity.
//
// The severity reaches the log LEVEL rather than only the message, so existing
// log-based alerting can filter on it without parsing our fields.
func (s *SlogNotifier) Notify(ctx context.Context, e Event) error {
	attrs := []any{"kind", e.Kind}
	if e.DedupKey != "" {
		attrs = append(attrs, "dedup_key", e.DedupKey)
	}
	for k, v := range e.Detail {
		attrs = append(attrs, k, v)
	}

	level := slog.LevelWarn
	if e.Severity == SeverityError {
		level = slog.LevelError
	}
	s.log.Log(ctx, level, e.Summary, attrs...)
	return nil
}

// ---------------------------------------------------------------------------
// webhook
// ---------------------------------------------------------------------------

// WebhookOptions configures a WebhookNotifier.
type WebhookOptions struct {
	// Timeout bounds one delivery attempt. Defaults to 10s. A bounded attempt
	// matters because a hanging endpoint could otherwise outlive the collector
	// tick that produced the event.
	Timeout time.Duration

	// HTTPClient is optional; one with Timeout is built when nil.
	HTTPClient *http.Client

	// Clock stamps the event. Injected so tests are deterministic and so the
	// timestamp is the service's notion of now — which is what a consumer
	// correlating an alert against a bill needs.
	Clock testutil.Clock

	// Headers are added to each request, for an endpoint needing a shared secret.
	Headers map[string]string
}

// WebhookNotifier POSTs events as JSON to a configured URL.
//
// Deliberately the only network transport here. Pointing it at ntfy, a Home
// Assistant webhook or an MQTT bridge is a configuration choice, so countinghouse
// never grows a dependency on whichever one is in use this year.
type WebhookNotifier struct {
	url     string
	client  *http.Client
	clock   testutil.Clock
	headers map[string]string
}

// NewWebhookNotifier returns a notifier posting to url.
func NewWebhookNotifier(url string, opts WebhookOptions) *WebhookNotifier {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: timeout}
	}
	clock := opts.Clock
	if clock == nil {
		clock = testutil.RealClock{}
	}
	return &WebhookNotifier{url: url, client: client, clock: clock, headers: opts.Headers}
}

// webhookPayload is the wire shape. Stable by intention: something downstream
// will be parsing it.
type webhookPayload struct {
	// Service names the sender so a shared endpoint can tell who called.
	Service  string         `json:"service"`
	Kind     string         `json:"kind"`
	Severity Severity       `json:"severity"`
	Summary  string         `json:"summary"`
	At       string         `json:"at"`
	DedupKey string         `json:"dedup_key,omitempty"`
	Detail   map[string]any `json:"detail,omitempty"`
}

// Notify posts the event. A non-2xx response or a transport failure is an error;
// the caller is expected to count it and carry on.
func (w *WebhookNotifier) Notify(ctx context.Context, e Event) error {
	body, err := json.Marshal(webhookPayload{
		Service:  "countinghouse",
		Kind:     e.Kind,
		Severity: e.Severity,
		Summary:  e.Summary,
		At:       w.clock.Now().UTC().Format("2006-01-02T15:04:05Z"),
		DedupKey: e.DedupKey,
		Detail:   e.Detail,
	})
	if err != nil {
		return fmt.Errorf("notify: marshal event: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("notify: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range w.headers {
		req.Header.Set(k, v)
	}

	resp, err := w.client.Do(req)
	if err != nil {
		// Surface a cancelled context unwrapped enough for errors.Is, so shutdown
		// is distinguishable from an endpoint being down.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("notify: post to webhook: %w", ctxErr)
		}
		return fmt.Errorf("notify: post to webhook: %w", err)
	}
	defer resp.Body.Close()                              //nolint:errcheck
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096)) //nolint:errcheck // drain to reuse the connection

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("notify: webhook returned %s", resp.Status)
	}
	return nil
}

// ---------------------------------------------------------------------------
// fan-out
// ---------------------------------------------------------------------------

// multi delivers to several notifiers.
type multi []Notifier

// Multi fans an event out to every notifier, continuing past a failure and
// joining the errors.
//
// Continuing matters: the slog notifier is normally one of these, so this is what
// guarantees an alert is still recorded when the webhook is down.
func Multi(ns ...Notifier) Notifier { return multi(ns) }

func (m multi) Notify(ctx context.Context, e Event) error {
	var errs []error
	for _, n := range m {
		if n == nil {
			continue
		}
		if err := n.Notify(ctx, e); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// ---------------------------------------------------------------------------
// throttling
// ---------------------------------------------------------------------------

// Throttle suppresses repeats of a condition that is already known to be true.
//
// This is what makes proactive alerting tolerable rather than self-defeating. The
// publication watch runs every few minutes, so a condition like "tomorrow's
// prices have not arrived" is true on many consecutive ticks. Delivering it each
// time produces dozens of identical messages in an evening, and the reliable
// outcome of that is a muted channel — which is worse than no alerting at all,
// because it then fails silently.
//
// So the rule is: alert on the TRANSITION into a condition, repeat only once per
// cooldown while it persists, and alert again immediately if it clears and
// returns (see Resolve). Safe for concurrent use.
type Throttle struct {
	inner    Notifier
	cooldown time.Duration
	clock    testutil.Clock

	mu       sync.Mutex
	lastSent map[throttleKey]time.Time
}

type throttleKey struct{ kind, dedup string }

// NewThrottle wraps inner, suppressing a repeated condition for cooldown.
// A cooldown of zero disables suppression, which is what tests of downstream
// behaviour want.
func NewThrottle(inner Notifier, cooldown time.Duration, clock testutil.Clock) *Throttle {
	if clock == nil {
		clock = testutil.RealClock{}
	}
	return &Throttle{
		inner:    inner,
		cooldown: cooldown,
		clock:    clock,
		lastSent: map[throttleKey]time.Time{},
	}
}

// Notify delivers the event unless the same condition was reported within the
// cooldown.
//
// A suppressed event returns nil: suppression is the intended outcome, not a
// failure, and a caller should not treat it as one.
func (t *Throttle) Notify(ctx context.Context, e Event) error {
	key := throttleKey{kind: e.Kind, dedup: e.DedupKey}
	now := t.clock.Now()

	t.mu.Lock()
	if t.cooldown > 0 {
		if last, seen := t.lastSent[key]; seen && now.Sub(last) < t.cooldown {
			t.mu.Unlock()
			return nil
		}
	}
	// Recorded BEFORE delivery and while still holding the lock, so concurrent
	// reports of the same condition cannot both get through.
	t.lastSent[key] = now
	t.mu.Unlock()

	return t.inner.Notify(ctx, e)
}

// Resolve records that a condition is no longer true, so its next occurrence is
// delivered immediately rather than waiting out the cooldown.
//
// Callers are expected to call this unconditionally whenever things look healthy,
// rather than tracking state themselves — resolving something that was never
// firing is harmless. Without it a flapping problem would be invisible after its
// first report, which is exactly the case worth seeing.
func (t *Throttle) Resolve(kind, dedupKey string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.lastSent, throttleKey{kind: kind, dedup: dedupKey})
}
