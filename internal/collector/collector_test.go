package collector

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/sweeney/countinghouse/internal/notify"
	"github.com/sweeney/countinghouse/internal/octopus"
	"github.com/sweeney/countinghouse/internal/prices"
	"github.com/sweeney/countinghouse/internal/testutil"
)

// ---------------------------------------------------------------------------
// The collector.
//
// Everything here is driven by a FakeClock and a fake fetcher, so a whole
// publication day — including a late publication, a missed one, and one that
// never completes — runs deterministically in microseconds. The real Octopus
// client is never used: its own failure modes are covered in internal/octopus,
// and what matters here is what the collector DOES about them.
//
// The tariff code uses region "A" throughout; nothing depends on a deployment's
// actual region.
// ---------------------------------------------------------------------------

const testTariffCode = "E-1R-AGILE-24-10-01-A"

func tariff(t *testing.T) octopus.TariffCode {
	t.Helper()
	tc, err := octopus.ParseTariffCode(testTariffCode)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return tc
}

func ts(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("bad time %q: %v", s, err)
	}
	return v.UTC()
}

func london(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Skipf("no tzdata: %v", err)
	}
	return loc
}

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

// fakeFetcher is a programmable stand-in for the Octopus client. It serves a
// contiguous run of half-hourly rates up to a settable horizon, which is how a
// test simulates "tomorrow has now been published".
type fakeFetcher struct {
	mu sync.Mutex

	// published is the exclusive end of what the supplier has published.
	published time.Time
	// earliest is where its history starts.
	earliest time.Time
	// priceAt lets a test control the price of a slot; nil means a fixed 20p.
	priceAt func(time.Time) (exc, inc float64)

	// failures, when non-empty, is popped on each UnitRates call.
	failures []error
	// horizonFailures, likewise, for the probe.
	horizonFailures []error

	rateCalls    []rateCall
	horizonCalls int
}

type rateCall struct{ from, to time.Time }

func newFakeFetcher(earliest, published time.Time) *fakeFetcher {
	return &fakeFetcher{earliest: earliest, published: published}
}

func (f *fakeFetcher) setPublished(t time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.published = t
}

func (f *fakeFetcher) calls() []rateCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]rateCall, len(f.rateCalls))
	copy(out, f.rateCalls)
	return out
}

func (f *fakeFetcher) Horizon(_ context.Context, _ octopus.TariffCode) (time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.horizonCalls++
	if len(f.horizonFailures) > 0 {
		err := f.horizonFailures[0]
		f.horizonFailures = f.horizonFailures[1:]
		return time.Time{}, err
	}
	return f.published, nil
}

func (f *fakeFetcher) UnitRates(_ context.Context, _ octopus.TariffCode, from, to time.Time) ([]octopus.Rate, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rateCalls = append(f.rateCalls, rateCall{from, to})
	// The real API answers 400 for an upper bound without a lower one. A fake that
	// accepted it would let that bug back in — and did, once: the hermetic suite
	// passed while the live fetch failed on a first sync.
	if from.IsZero() && !to.IsZero() {
		return nil, fmt.Errorf("fake: period_to without period_from is rejected by the real API")
	}
	if len(f.failures) > 0 {
		err := f.failures[0]
		f.failures = f.failures[1:]
		return nil, err
	}

	start := f.earliest
	if !from.IsZero() && from.After(start) {
		start = from
	}
	end := f.published
	if !to.IsZero() && to.Before(end) {
		end = to
	}
	// Align the start up to a half-hour boundary.
	if r := start.Truncate(30 * time.Minute); !r.Equal(start) {
		start = r.Add(30 * time.Minute)
	}

	var out []octopus.Rate
	for cur := start; cur.Before(end); cur = cur.Add(30 * time.Minute) {
		validTo := cur.Add(30 * time.Minute)
		exc, inc := 20.0, 21.0
		if f.priceAt != nil {
			exc, inc = f.priceAt(cur)
		}
		vt := validTo
		out = append(out, octopus.Rate{
			ValidFrom: cur, ValidTo: &vt, ExcVATPence: exc, IncVATPence: inc,
		})
	}
	return out, nil
}

// recordingNotifier captures events for assertion.
type recordingNotifier struct {
	mu     sync.Mutex
	events []notify.Event
}

func (r *recordingNotifier) Notify(_ context.Context, e notify.Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
	return nil
}

func (r *recordingNotifier) kinds() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.events))
	for _, e := range r.events {
		out = append(out, e.Kind)
	}
	return out
}

func (r *recordingNotifier) has(kind string) bool {
	for _, k := range r.kinds() {
		if k == kind {
			return true
		}
	}
	return false
}

// harness wires a collector to an in-memory archive.
type harness struct {
	c     *Collector
	store *prices.SQLiteStore
	fetch *fakeFetcher
	noti  *recordingNotifier
	clock *testutil.FakeClock
}

func newHarness(t *testing.T, now time.Time, f *fakeFetcher) *harness {
	t.Helper()
	store, err := prices.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { store.Close() }) //nolint:errcheck

	clock := testutil.NewFakeClock(now)
	noti := &recordingNotifier{}
	c, err := New(Options{
		Fetcher:    f,
		Store:      store,
		Notifier:   noti,
		Clock:      clock,
		Location:   london(t),
		TariffCode: testTariffCode,
		VATRate:    0.05,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return &harness{c: c, store: store, fetch: f, noti: noti, clock: clock}
}

// ---------------------------------------------------------------------------
// Construction
// ---------------------------------------------------------------------------

// A collector with a missing dependency must fail at construction. Starting one
// that silently does nothing is the worst outcome: the archive stops filling and
// /healthz has nothing to complain about.
func TestNewRejectsIncompleteOptions(t *testing.T) {
	store, err := prices.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close() //nolint:errcheck

	valid := Options{
		Fetcher: newFakeFetcher(time.Time{}, time.Time{}), Store: store,
		Clock: testutil.NewFakeClock(time.Now()), Location: time.UTC,
		TariffCode: testTariffCode,
	}

	for _, tc := range []struct {
		name  string
		mutar func(*Options)
	}{
		{name: "no fetcher", mutar: func(o *Options) { o.Fetcher = nil }},
		{name: "no store", mutar: func(o *Options) { o.Store = nil }},
		{name: "no tariff code", mutar: func(o *Options) { o.TariffCode = "" }},
		{name: "unparseable tariff code", mutar: func(o *Options) { o.TariffCode = "nonsense" }},
		{
			// Without a zone the local-day arithmetic is meaningless: in UTC no
			// day is ever 46 or 50 slots, so DST completeness would be wrong
			// twice a year and look fine the rest of the time.
			name: "no location", mutar: func(o *Options) { o.Location = nil },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := valid
			tc.mutar(&opts)
			if _, err := New(opts); err == nil {
				t.Error("want an error rather than a collector that quietly does nothing")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Sync: the core fetch-validate-store cycle
// ---------------------------------------------------------------------------

// An empty archive must be filled from the supplier's earliest published slot.
func TestSyncFillsAnEmptyArchive(t *testing.T) {
	// Published through the end of local 2026-09-11 (23:00 BST = 22:00Z).
	f := newFakeFetcher(ts(t, "2026-09-09T23:00:00Z"), ts(t, "2026-09-11T22:00:00Z"))
	h := newHarness(t, ts(t, "2026-09-10T16:10:00Z"), f)
	ctx := context.Background()

	res, err := h.c.Sync(ctx)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if res.Stored.Inserted == 0 {
		t.Fatalf("stored nothing: %+v", res)
	}

	known, err := h.store.KnownTo(ctx, testTariffCode)
	if err != nil {
		t.Fatal(err)
	}
	if want := ts(t, "2026-09-11T22:00:00Z"); !known.Equal(want) {
		t.Errorf("KnownTo = %s, want %s", known, want)
	}
}

// Once the archive already holds everything published, a sync must cost ONE
// request — the horizon probe — and fetch no rates at all. The watch runs every
// few minutes; re-downloading a day each time would be gratuitous load on a
// public API that rate-limits.
func TestSyncWithNothingNewFetchesNoRates(t *testing.T) {
	f := newFakeFetcher(ts(t, "2026-09-09T23:00:00Z"), ts(t, "2026-09-11T22:00:00Z"))
	h := newHarness(t, ts(t, "2026-09-10T16:10:00Z"), f)
	ctx := context.Background()

	if _, err := h.c.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	before := len(f.calls())

	res, err := h.c.Sync(ctx)
	if err != nil {
		t.Fatalf("second Sync: %v", err)
	}
	if got := len(f.calls()); got != before {
		t.Errorf("made %d rate requests on the second sync, want 0 more than %d", got-before, before)
	}
	if res.Stored.Inserted != 0 {
		t.Errorf("inserted %d on a no-op sync", res.Stored.Inserted)
	}
	if !res.UpToDate {
		t.Error("UpToDate should be true when the horizon has not moved")
	}
}

// When the horizon advances — the daily publication — only the NEW range is
// fetched, not the whole history.
func TestSyncFetchesOnlyTheNewRange(t *testing.T) {
	f := newFakeFetcher(ts(t, "2026-09-09T23:00:00Z"), ts(t, "2026-09-10T22:00:00Z"))
	h := newHarness(t, ts(t, "2026-09-10T14:00:00Z"), f)
	ctx := context.Background()

	if _, err := h.c.Sync(ctx); err != nil {
		t.Fatal(err)
	}

	// 16:00 arrives and tomorrow is published.
	f.setPublished(ts(t, "2026-09-11T22:00:00Z"))
	h.clock.Set(ts(t, "2026-09-10T16:05:00Z"))

	before := len(f.calls())
	res, err := h.c.Sync(ctx)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if res.Stored.Inserted != 48 {
		t.Errorf("inserted %d, want the 48 newly published slots", res.Stored.Inserted)
	}

	calls := f.calls()
	if len(calls) != before+1 {
		t.Fatalf("made %d rate requests, want exactly 1 more", len(calls)-before)
	}
	// The fetch resumes near the existing horizon rather than at the beginning of
	// time. It deliberately reaches back by fetchOverlap — writes are idempotent,
	// and that is cheap insurance against a slot missed at a boundary being
	// skipped forever once the horizon moved past it.
	last := calls[len(calls)-1]
	earliestAcceptable := ts(t, "2026-09-10T22:00:00Z").Add(-fetchOverlap)
	if last.from.Before(earliestAcceptable) {
		t.Errorf("refetched from %s; should have resumed no earlier than %s (horizon minus the %v overlap)",
			last.from, earliestAcceptable, fetchOverlap)
	}
}

// Re-syncing the same data must not look like a change. This is the property
// that makes the overlapping sweeps safe, so it is asserted end to end rather
// than only at the store.
func TestSyncIsIdempotentAndRaisesNoFalseRestatement(t *testing.T) {
	f := newFakeFetcher(ts(t, "2026-09-09T23:00:00Z"), ts(t, "2026-09-11T22:00:00Z"))
	h := newHarness(t, ts(t, "2026-09-10T16:10:00Z"), f)
	ctx := context.Background()

	if _, err := h.c.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	// Force a full re-fetch of a range we already hold.
	res, err := h.c.Backfill(ctx, ts(t, "2026-09-09T23:00:00Z"), ts(t, "2026-09-11T22:00:00Z"))
	if err != nil {
		t.Fatalf("Backfill: %v", err)
	}
	if res.Stored.Restated != 0 {
		t.Errorf("re-fetching identical data reported %d restatements", res.Stored.Restated)
	}
	if res.Stored.Inserted != 0 {
		t.Errorf("re-fetching identical data inserted %d rows", res.Stored.Inserted)
	}
	if h.noti.has(notify.KindRestatement) {
		t.Error("a re-fetch of identical data must not alert")
	}
}

// ---------------------------------------------------------------------------
// Validation outcomes
// ---------------------------------------------------------------------------

// A slot the gates reject must not reach the archive, and must not be silent.
func TestSyncQuarantinesRejectedSlotsAndAlerts(t *testing.T) {
	f := newFakeFetcher(ts(t, "2026-09-10T00:00:00Z"), ts(t, "2026-09-10T02:00:00Z"))
	// Break the VAT relationship on one slot: inc is not exc x 1.05.
	bad := ts(t, "2026-09-10T01:00:00Z")
	f.priceAt = func(at time.Time) (float64, float64) {
		if at.Equal(bad) {
			return 20, 99
		}
		return 20, 21
	}
	h := newHarness(t, ts(t, "2026-09-10T16:10:00Z"), f)
	ctx := context.Background()

	res, err := h.c.Sync(ctx)
	if err != nil {
		t.Fatalf("Sync should still store the good slots: %v", err)
	}
	if len(res.Rejected) != 1 {
		t.Fatalf("rejected %d slots, want 1: %+v", len(res.Rejected), res.Rejected)
	}
	if res.Stored.Inserted != 3 {
		t.Errorf("inserted %d, want the 3 valid slots", res.Stored.Inserted)
	}

	// The bad slot must be absent from the archive.
	held, err := h.store.Range(ctx, testTariffCode, bad, bad.Add(30*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(held) != 0 {
		t.Errorf("a rejected slot reached the archive: %+v", held)
	}
	if !h.noti.has(notify.KindValidationRejected) {
		t.Errorf("a rejection must alert; got kinds %v", h.noti.kinds())
	}
}

// A supplier revising a price we already hold is the event most worth knowing
// about: a bill we may already have issued has moved.
func TestSyncAlertsOnRestatement(t *testing.T) {
	f := newFakeFetcher(ts(t, "2026-09-10T00:00:00Z"), ts(t, "2026-09-10T01:00:00Z"))
	h := newHarness(t, ts(t, "2026-09-10T16:10:00Z"), f)
	ctx := context.Background()

	if _, err := h.c.Sync(ctx); err != nil {
		t.Fatal(err)
	}

	// The supplier now reports a different price for the same slots.
	f.priceAt = func(time.Time) (float64, float64) { return 30, 31.5 }
	h.clock.Advance(48 * time.Hour)
	res, err := h.c.Backfill(ctx, ts(t, "2026-09-10T00:00:00Z"), ts(t, "2026-09-10T01:00:00Z"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Stored.Restated != 2 {
		t.Errorf("restated %d, want 2", res.Stored.Restated)
	}
	if !h.noti.has(notify.KindRestatement) {
		t.Errorf("a restatement must alert; got %v", h.noti.kinds())
	}

	rs, err := h.store.Restatements(ctx, testTariffCode, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 2 {
		t.Errorf("archive recorded %d restatements, want 2", len(rs))
	}
}

// ---------------------------------------------------------------------------
// Fail-open
// ---------------------------------------------------------------------------

// A failing supplier must leave the archive exactly as it was, report the error,
// and never write a guess. The whole design assumes this, so it is asserted for
// each failure shape the client can hand us.
func TestSyncIsFailOpen(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "service unavailable", err: &octopus.APIError{StatusCode: http.StatusServiceUnavailable, Status: "503"}},
		{name: "rate limited", err: &octopus.APIError{StatusCode: http.StatusTooManyRequests, Status: "429"}},
		{name: "forbidden", err: &octopus.APIError{StatusCode: http.StatusForbidden, Status: "403"}},
		{name: "transport failure", err: errors.New("dial tcp: connection refused")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeFetcher(ts(t, "2026-09-09T23:00:00Z"), ts(t, "2026-09-11T22:00:00Z"))
			h := newHarness(t, ts(t, "2026-09-10T16:10:00Z"), f)
			ctx := context.Background()

			// Seed a known-good day first.
			if _, err := h.c.Sync(ctx); err != nil {
				t.Fatal(err)
			}
			knownBefore, err := h.store.KnownTo(ctx, testTariffCode)
			if err != nil {
				t.Fatal(err)
			}

			successBefore := h.c.Status().LastSuccess

			// Now the supplier breaks, and claims to have more. The clock moves so
			// a stale LastSuccess is distinguishable from a fresh one.
			h.clock.Advance(5 * time.Minute)
			f.setPublished(ts(t, "2026-09-12T23:00:00Z"))
			f.failures = []error{tc.err}

			if _, err := h.c.Sync(ctx); err == nil {
				t.Fatal("want the error surfaced so the caller can record it")
			}
			if got := h.c.Status().LastSuccess; !got.Equal(successBefore) {
				t.Errorf("LastSuccess moved to %s during a failure; health would look fine", got)
			}

			knownAfter, err := h.store.KnownTo(ctx, testTariffCode)
			if err != nil {
				t.Fatal(err)
			}
			if !knownAfter.Equal(knownBefore) {
				t.Errorf("archive moved from %s to %s during a failed sync", knownBefore, knownAfter)
			}
			if h.c.Status().LastError == "" {
				t.Error("the failure should be visible on Status for /healthz")
			}
			if st := h.c.Status(); !st.LastSuccess.Before(st.LastAttempt) {
				t.Errorf("LastSuccess %s should now lag LastAttempt %s", st.LastSuccess, st.LastAttempt)
			}
		})
	}
}

// A failing horizon probe is the same story: nothing written, error surfaced.
func TestSyncFailOpenOnHorizonProbe(t *testing.T) {
	f := newFakeFetcher(ts(t, "2026-09-09T23:00:00Z"), ts(t, "2026-09-11T22:00:00Z"))
	f.horizonFailures = []error{&octopus.APIError{StatusCode: 503, Status: "503"}}
	h := newHarness(t, ts(t, "2026-09-10T16:10:00Z"), f)

	if _, err := h.c.Sync(context.Background()); err == nil {
		t.Fatal("want an error when the probe fails")
	}
	if len(f.calls()) != 0 {
		t.Errorf("fetched rates despite a failed probe: %+v", f.calls())
	}
}

// A failure followed by a success must clear the error, or /healthz would stay
// red after the problem has gone.
func TestSyncRecoversAfterFailure(t *testing.T) {
	f := newFakeFetcher(ts(t, "2026-09-09T23:00:00Z"), ts(t, "2026-09-11T22:00:00Z"))
	f.failures = []error{errors.New("transient")}
	h := newHarness(t, ts(t, "2026-09-10T16:10:00Z"), f)
	ctx := context.Background()

	if _, err := h.c.Sync(ctx); err == nil {
		t.Fatal("expected the seeded failure")
	}
	if h.c.Status().LastError == "" {
		t.Fatal("error not recorded")
	}

	h.clock.Advance(5 * time.Minute)
	if _, err := h.c.Sync(ctx); err != nil {
		t.Fatalf("second sync should succeed: %v", err)
	}
	if got := h.c.Status().LastError; got != "" {
		t.Errorf("LastError = %q, want it cleared after a success", got)
	}
}

// ---------------------------------------------------------------------------
// Completeness: the observed partial-day case
// ---------------------------------------------------------------------------

// The real 2026-09-10 case. The horizon covers all of tomorrow while tomorrow is
// missing its last two slots. complete_to must NOT advance past the
// incomplete day, and the collector must report it as still worth polling rather
// than alerting immediately.
func TestSyncDistinguishesKnownToFromCompleteTo(t *testing.T) {
	// Exactly the observed case: published through 22:00Z, so the last slot is
	// [21:30,22:00) and local 2026-09-11 is missing its final two — 46 of 48.
	f := newFakeFetcher(ts(t, "2026-09-09T23:00:00Z"), ts(t, "2026-09-11T22:00:00Z"))
	h := newHarness(t, ts(t, "2026-09-10T16:08:00Z"), f)
	ctx := context.Background()

	res, err := h.c.Sync(ctx)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}

	st := h.c.Status()
	if !st.KnownTo.Equal(ts(t, "2026-09-11T22:00:00Z")) {
		t.Errorf("KnownTo = %s, want the newest slot's end", st.KnownTo)
	}
	// The incomplete day must not count as complete.
	if st.CompleteTo.After(ts(t, "2026-09-10T23:00:00Z")) {
		t.Errorf("CompleteTo = %s; it must not advance past an incomplete day", st.CompleteTo)
	}
	if res.TomorrowComplete {
		t.Error("tomorrow is missing two slots and must not be reported complete")
	}
	// A tail gap during the publication window means keep polling, not alert.
	if h.noti.has(notify.KindDayIncomplete) {
		t.Error("a tail gap inside the publication window should not alert yet")
	}
	if !res.KeepPolling {
		t.Error("KeepPolling should be true while the tail is still filling")
	}
}

// Once the day is filled, it is complete and polling stops.
func TestSyncCompletesWhenTheTailArrives(t *testing.T) {
	f := newFakeFetcher(ts(t, "2026-09-09T23:00:00Z"), ts(t, "2026-09-11T22:00:00Z"))
	h := newHarness(t, ts(t, "2026-09-10T16:08:00Z"), f)
	ctx := context.Background()

	if _, err := h.c.Sync(ctx); err != nil {
		t.Fatal(err)
	}

	// The missing tail lands twenty minutes later, taking the day to its local end.
	f.setPublished(ts(t, "2026-09-11T23:00:00Z"))
	h.clock.Advance(20 * time.Minute)

	res, err := h.c.Sync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !res.TomorrowComplete {
		t.Errorf("tomorrow should now be complete: %+v", res)
	}
	if res.KeepPolling {
		t.Error("KeepPolling should be false once tomorrow is complete")
	}
	if st := h.c.Status(); !st.CompleteTo.Equal(ts(t, "2026-09-11T23:00:00Z")) {
		t.Errorf("CompleteTo = %s, want the end of the now-complete day", st.CompleteTo)
	}
}

// Both DST day lengths must be recognised as complete. A checker assuming 48
// would call one short and the other overfull, twice a year.
func TestSyncHandlesDSTDayLengths(t *testing.T) {
	for _, tc := range []struct {
		name             string
		now, from, until string
		slots            int
	}{
		{
			name: "spring forward: a 23h local day is 46 slots",
			now:  "2026-03-28T16:10:00Z",
			from: "2026-03-28T00:00:00Z", until: "2026-03-29T23:00:00Z", slots: 46,
		},
		{
			name: "autumn back: a 25h local day is 50 slots",
			now:  "2025-10-25T16:10:00Z",
			from: "2025-10-25T23:00:00Z", until: "2025-10-27T00:00:00Z", slots: 50,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeFetcher(ts(t, tc.from), ts(t, tc.until))
			h := newHarness(t, ts(t, tc.now), f)
			ctx := context.Background()

			res, err := h.c.Sync(ctx)
			if err != nil {
				t.Fatalf("Sync: %v", err)
			}
			if !res.TomorrowComplete {
				t.Errorf("the %d-slot DST day should be complete: %+v", tc.slots, res)
			}
		})
	}
}

// A day still incomplete after the deadline stops being "wait" and becomes
// "tell somebody": tomorrow's costing will have holes in it.
func TestSyncAlertsOnDayStillIncompleteAtDeadline(t *testing.T) {
	f := newFakeFetcher(ts(t, "2026-09-09T23:00:00Z"), ts(t, "2026-09-11T22:00:00Z"))
	h := newHarness(t, ts(t, "2026-09-10T16:08:00Z"), f)
	ctx := context.Background()

	if _, err := h.c.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if h.noti.has(notify.KindDayIncomplete) {
		t.Fatal("too early to alert")
	}

	// 23:00 local has come and gone and the tail never arrived.
	h.clock.Set(ts(t, "2026-09-10T22:30:00Z"))
	res, err := h.c.Sync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !h.noti.has(notify.KindDayIncomplete) {
		t.Errorf("past the deadline an incomplete day must alert; got %v", h.noti.kinds())
	}
	if res.KeepPolling {
		t.Error("KeepPolling should be false past the deadline")
	}
}

// Before the publication window, tomorrow legitimately does not exist yet. That
// is the normal state for most of the day and must never alert.
func TestSyncDoesNotAlertBeforeThePublicationWindow(t *testing.T) {
	f := newFakeFetcher(ts(t, "2026-09-09T23:00:00Z"), ts(t, "2026-09-10T23:00:00Z"))
	h := newHarness(t, ts(t, "2026-09-10T09:00:00Z"), f)

	res, err := h.c.Sync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.TomorrowComplete {
		t.Error("tomorrow is not published yet; it cannot be complete")
	}
	if h.noti.has(notify.KindDayIncomplete) || h.noti.has(notify.KindPricesMissing) {
		t.Errorf("the morning state must be silent; got %v", h.noti.kinds())
	}
}

// TODAY having no prices is a different and much worse condition than tomorrow
// not being published: energy is being consumed right now that cannot be priced.
func TestSyncAlertsWhenTodayHasNoPrices(t *testing.T) {
	// The supplier has published nothing at all.
	f := newFakeFetcher(time.Time{}, time.Time{})
	h := newHarness(t, ts(t, "2026-09-10T12:00:00Z"), f)

	if _, err := h.c.Sync(context.Background()); err != nil {
		t.Fatalf("an empty supplier is not an error: %v", err)
	}
	if !h.noti.has(notify.KindPricesMissing) {
		t.Errorf("no prices for today must alert; got %v", h.noti.kinds())
	}
}

// ---------------------------------------------------------------------------
// Sweeps and scheduling
// ---------------------------------------------------------------------------

// The catch-up sweep re-reads recent days so an outage heals itself, and must be
// free of side effects when nothing was lost.
func TestCatchUpSweepHealsAGapAndIsOtherwiseQuiet(t *testing.T) {
	f := newFakeFetcher(ts(t, "2026-09-01T23:00:00Z"), ts(t, "2026-09-11T22:00:00Z"))
	h := newHarness(t, ts(t, "2026-09-10T16:10:00Z"), f)
	ctx := context.Background()

	// Seed only the most recent day, leaving earlier days absent.
	if _, err := h.c.Backfill(ctx, ts(t, "2026-09-10T23:00:00Z"), ts(t, "2026-09-11T22:00:00Z")); err != nil {
		t.Fatal(err)
	}

	res, err := h.c.CatchUp(ctx, 7)
	if err != nil {
		t.Fatalf("CatchUp: %v", err)
	}
	if res.Stored.Inserted == 0 {
		t.Error("the sweep should have filled the earlier days")
	}
	if res.Stored.Restated != 0 {
		t.Errorf("the sweep reported %d restatements over unchanged data", res.Stored.Restated)
	}

	// Running it again changes nothing.
	second, err := h.c.CatchUp(ctx, 7)
	if err != nil {
		t.Fatal(err)
	}
	if second.Stored.Inserted != 0 || second.Stored.Restated != 0 {
		t.Errorf("a repeat sweep was not a no-op: %+v", second.Stored)
	}
}

// DueIn tells the caller when to come back. It is the scheduling policy, kept
// as a pure function of the clock and the archive so it can be asserted without
// running a loop.
func TestDueInReflectsTheSchedule(t *testing.T) {
	for _, tc := range []struct {
		name      string
		now       string
		published string
		wantMax   time.Duration
		wantMin   time.Duration
	}{
		{
			// Inside the publication window with tomorrow not yet complete:
			// check back soon.
			name: "watching for the publication", now: "2026-09-10T15:50:00Z",
			published: "2026-09-10T23:00:00Z",
			wantMin:   time.Minute, wantMax: 10 * time.Minute,
		},
		{
			// Tomorrow is complete: nothing more to wait for today, so back off.
			name: "tomorrow already complete", now: "2026-09-10T16:30:00Z",
			published: "2026-09-11T23:00:00Z",
			wantMin:   30 * time.Minute, wantMax: 2 * time.Hour,
		},
		{
			// Middle of the morning, nothing expected: back off.
			name: "outside the window", now: "2026-09-10T09:00:00Z",
			published: "2026-09-10T23:00:00Z",
			wantMin:   30 * time.Minute, wantMax: 2 * time.Hour,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeFetcher(ts(t, "2026-09-09T23:00:00Z"), ts(t, tc.published))
			h := newHarness(t, ts(t, tc.now), f)
			ctx := context.Background()
			if _, err := h.c.Sync(ctx); err != nil {
				t.Fatal(err)
			}

			got := h.c.DueIn()
			if got < tc.wantMin || got > tc.wantMax {
				t.Errorf("DueIn = %v, want between %v and %v", got, tc.wantMin, tc.wantMax)
			}
		})
	}
}

// A whole publication day, driven by the clock: quiet all morning, the watch
// picks up the publication, and it settles once the day is complete. This is the
// test that proves the schedule works without waiting for real time.
func TestFakeClockDrivesAWholePublicationDay(t *testing.T) {
	f := newFakeFetcher(ts(t, "2026-09-09T23:00:00Z"), ts(t, "2026-09-10T23:00:00Z"))
	h := newHarness(t, ts(t, "2026-09-10T06:00:00Z"), f)
	ctx := context.Background()

	// Morning: today is known, tomorrow is not published. Quiet.
	if _, err := h.c.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if h.noti.has(notify.KindPricesMissing) {
		t.Fatalf("the morning should be silent; got %v", h.noti.kinds())
	}

	// Tick through the afternoon. Nothing new until the supplier publishes.
	for _, at := range []string{"2026-09-10T10:00:00Z", "2026-09-10T14:00:00Z", "2026-09-10T14:50:00Z"} {
		h.clock.Set(ts(t, at))
		res, err := h.c.Sync(ctx)
		if err != nil {
			t.Fatalf("sync at %s: %v", at, err)
		}
		if res.Stored.Inserted != 0 {
			t.Errorf("at %s: stored %d slots before publication", at, res.Stored.Inserted)
		}
	}

	// ~16:00 local: tomorrow lands, but two slots short.
	h.clock.Set(ts(t, "2026-09-10T15:05:00Z"))
	f.setPublished(ts(t, "2026-09-11T22:00:00Z"))
	res, err := h.c.Sync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Stored.Inserted != 46 {
		t.Errorf("stored %d slots at publication, want 46", res.Stored.Inserted)
	}
	if res.TomorrowComplete {
		t.Error("46 of 48 is not complete")
	}
	if !res.KeepPolling {
		t.Error("should still be polling for the tail")
	}

	// The tail arrives.
	h.clock.Set(ts(t, "2026-09-10T15:35:00Z"))
	f.setPublished(ts(t, "2026-09-11T23:00:00Z"))
	res, err = h.c.Sync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Stored.Inserted != 2 {
		t.Errorf("stored %d slots for the tail, want 2", res.Stored.Inserted)
	}
	if !res.TomorrowComplete {
		t.Error("the day should now be complete")
	}
	if res.KeepPolling {
		t.Error("polling should stop once complete")
	}

	// Nothing was ever alerted across the whole normal day.
	if kinds := h.noti.kinds(); len(kinds) != 0 {
		t.Errorf("a normal publication day produced alerts: %v", kinds)
	}
}

// A publication that never arrives must end in an alert rather than silence.
func TestFakeClockDrivesAMissedPublication(t *testing.T) {
	f := newFakeFetcher(ts(t, "2026-09-09T23:00:00Z"), ts(t, "2026-09-10T23:00:00Z"))
	h := newHarness(t, ts(t, "2026-09-10T15:05:00Z"), f)
	ctx := context.Background()

	// Tick repeatedly through the evening; the supplier never publishes.
	for at := ts(t, "2026-09-10T15:05:00Z"); at.Before(ts(t, "2026-09-10T23:00:00Z")); at = at.Add(30 * time.Minute) {
		h.clock.Set(at)
		if _, err := h.c.Sync(ctx); err != nil {
			t.Fatalf("sync at %s: %v", at, err)
		}
	}

	if !h.noti.has(notify.KindPricesMissing) && !h.noti.has(notify.KindDayIncomplete) {
		t.Errorf("a missed publication must eventually alert; got %v", h.noti.kinds())
	}
}

// Run must return promptly when its context is cancelled, or shutdown would
// exceed the server's graceful-shutdown budget.
func TestRunStopsOnContextCancel(t *testing.T) {
	f := newFakeFetcher(ts(t, "2026-09-09T23:00:00Z"), ts(t, "2026-09-11T22:00:00Z"))
	h := newHarness(t, ts(t, "2026-09-10T16:10:00Z"), f)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { h.c.Run(ctx); close(done) }()

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after its context was cancelled")
	}
}

// Status is what /healthz renders, so its counters must accumulate across syncs
// rather than describing only the last one.
func TestStatusCountersAccumulate(t *testing.T) {
	f := newFakeFetcher(ts(t, "2026-09-10T00:00:00Z"), ts(t, "2026-09-10T02:00:00Z"))
	bad := ts(t, "2026-09-10T01:00:00Z")
	f.priceAt = func(at time.Time) (float64, float64) {
		if at.Equal(bad) {
			return 20, 99 // breaks the VAT relationship
		}
		return 20, 21
	}
	h := newHarness(t, ts(t, "2026-09-10T16:10:00Z"), f)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		h.clock.Advance(time.Minute)
		if _, err := h.c.Backfill(ctx, ts(t, "2026-09-10T00:00:00Z"), ts(t, "2026-09-10T02:00:00Z")); err != nil {
			t.Fatal(err)
		}
	}

	st := h.c.Status()
	if st.Rejected != 3 {
		t.Errorf("Rejected = %d, want 3 accumulated across three syncs", st.Rejected)
	}
	if st.TariffCode != testTariffCode {
		t.Errorf("TariffCode = %q", st.TariffCode)
	}
	if st.LastSuccess.IsZero() {
		t.Error("LastSuccess should be set")
	}
}

// Backfill walks the whole published history in bounded chunks, so a first run
// does not depend on one enormous response.
func TestBackfillChunksALongRange(t *testing.T) {
	f := newFakeFetcher(ts(t, "2026-01-01T00:00:00Z"), ts(t, "2026-03-01T00:00:00Z"))
	h := newHarness(t, ts(t, "2026-09-10T16:10:00Z"), f)

	res, err := h.c.Backfill(context.Background(), ts(t, "2026-01-01T00:00:00Z"), ts(t, "2026-03-01T00:00:00Z"))
	if err != nil {
		t.Fatalf("Backfill: %v", err)
	}
	// 59 days of half hours.
	if want := 59 * 48; res.Stored.Inserted != want {
		t.Errorf("inserted %d, want %d", res.Stored.Inserted, want)
	}
	if n := len(f.calls()); n < 2 {
		t.Errorf("made %d requests for a two-month range; it should be chunked", n)
	}
	t.Logf("backfilled %d slots in %d requests", res.Stored.Inserted, len(f.calls()))
}

var _ = fmt.Sprintf // keep fmt for debugging helpers

// Plunge pricing must survive the whole pipeline, not just the layers that parse
// and store it. The octopus and prices packages each assert negatives
// individually; this is the end-to-end path — fetch, validate, store — with a day
// that is mostly negative, which is the shape of a real oversupplied day.
//
// It matters because a negative price is the one value most likely to trip
// something incidental: a sum that assumes monotonic growth, a comparison that
// assumes positive money, a threshold that fires on anything unusual. Those would
// all pass a test suite built only on 20p slots.
func TestSyncHandlesAPlungePricingDay(t *testing.T) {
	// earliest covers TODAY too (local 2026-04-10 starts at 2026-04-09T23:00Z);
	// otherwise the collector correctly reports today as unpriced and this test
	// would be asserting against that instead of against plunge handling.
	f := newFakeFetcher(ts(t, "2026-04-09T23:00:00Z"), ts(t, "2026-04-11T23:00:00Z"))
	// Modelled on the observed 2026-04-11: 36 of 48 slots negative, minimum
	// -10.69p, with VAT making each MORE negative.
	f.priceAt = func(at time.Time) (float64, float64) {
		exc := -10.69
		if h := at.UTC().Hour(); h >= 17 && h < 23 {
			exc = 15.15 // the evening peak is still positive
		}
		return exc, exc * 1.05
	}
	h := newHarness(t, ts(t, "2026-04-10T16:10:00Z"), f)
	ctx := context.Background()

	res, err := h.c.Sync(ctx)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if res.Stored.Inserted == 0 {
		t.Fatal("stored nothing")
	}
	// Nothing about a negative price is invalid, and nothing about it is even
	// surprising enough to warn: being paid to consume is the point of the tariff.
	if len(res.Rejected) != 0 {
		t.Errorf("rejected %d slots of a legitimate plunge day: %+v", len(res.Rejected), res.Rejected)
	}
	if len(res.Warnings) != 0 {
		t.Errorf("warned on %d slots; -10.69p is well inside the observed range: %+v",
			len(res.Warnings), res.Warnings)
	}
	if !res.TomorrowComplete {
		t.Errorf("the day should be complete: %+v", res)
	}
	if kinds := h.noti.kinds(); len(kinds) != 0 {
		t.Errorf("a plunge day raised alerts: %v", kinds)
	}

	// The archive must hold them still negative, and with VAT more negative than
	// ex-VAT — the sign-aware relationship, not abs().
	held, err := h.store.Range(ctx, testTariffCode,
		ts(t, "2026-04-11T00:00:00Z"), ts(t, "2026-04-11T06:00:00Z"))
	if err != nil {
		t.Fatal(err)
	}
	if len(held) == 0 {
		t.Fatal("no slots held for the negative stretch")
	}
	var negatives int
	for _, s := range held {
		if s.ExcVATPence >= 0 {
			continue
		}
		negatives++
		if s.IncVATPence >= s.ExcVATPence {
			t.Errorf("slot %s: inc %v should be MORE negative than exc %v",
				s.ValidFrom.Format(time.RFC3339), s.IncVATPence, s.ExcVATPence)
		}
	}
	if negatives == 0 {
		t.Error("no negative slots reached the archive; the fixture is not testing what it claims")
	}
	t.Logf("%d negative slots round-tripped through the collector", negatives)
}
