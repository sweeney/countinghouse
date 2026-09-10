// Package collector keeps the price archive up to date.
//
// It is the only part of countinghouse that writes, and it writes only the
// external price facts described in CLAUDE.md's amended invariant.
//
// The design principle throughout is **fail-open on fetching, fail-loud on
// pricing**. A fetch that does not work leaves the archive exactly as it was,
// records why, and lets health degrade — it never writes a guess, because a
// plausible wrong price is worse than a visible gap. Conversely a gap that will
// affect billing is pushed at somebody rather than left on /healthz for whenever
// they next look.
//
// Scheduling is separated from work: Sync does one unit of work and DueIn says
// when to come back, both pure functions of the clock and the archive. Run is a
// thin loop over the two. That split is what lets a test drive an entire
// publication day — including a late publication and one that never arrives —
// deterministically and in microseconds.
package collector

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/sweeney/countinghouse/internal/notify"
	"github.com/sweeney/countinghouse/internal/octopus"
	"github.com/sweeney/countinghouse/internal/prices"
	"github.com/sweeney/countinghouse/internal/testutil"
)

const (
	// watchFromLocalHour is when the publication watch starts. Prices for the
	// next day appear around 16:00 UK; the watch opens a little early so a
	// punctual publication is picked up on the first tick rather than the second.
	watchFromLocalHour = 15

	// watchDeadlineLocalHour is when waiting stops being reasonable. Past this,
	// a day that is still incomplete is no longer "the publication is landing" —
	// it is a gap that will affect tomorrow's costing, and somebody is told.
	watchDeadlineLocalHour = 23

	// watchInterval is how often to look while actively waiting for a
	// publication. Each check is a single ~350-byte request.
	watchInterval = 5 * time.Minute

	// idleInterval is the backstop cadence when nothing is expected. It exists
	// so a publication that somehow misses the watch window is still noticed.
	idleInterval = time.Hour

	// backfillChunk bounds one request's span, so a first run does not depend on
	// a single enormous response and a failure costs only one chunk.
	backfillChunk = 30 * 24 * time.Hour

	// fetchOverlap is how far back before the known horizon a routine sync
	// re-reads. Cheap insurance: writes are idempotent, and it means a slot that
	// was somehow missed at a boundary gets picked up rather than being skipped
	// forever because the horizon moved past it.
	fetchOverlap = 2 * time.Hour
)

// RateFetcher is the slice of the Octopus client this package needs.
//
// An interface rather than the concrete client so tests can drive a whole
// publication day without a network, and so the client's own failure modes stay
// tested where they belong (internal/octopus) instead of being re-simulated here.
// *octopus.Client satisfies it.
type RateFetcher interface {
	Horizon(ctx context.Context, tariff octopus.TariffCode) (time.Time, error)
	UnitRates(ctx context.Context, tariff octopus.TariffCode, from, to time.Time) ([]octopus.Rate, error)
}

// Options configures a Collector.
type Options struct {
	Fetcher  RateFetcher
	Store    prices.Store
	Notifier notify.Notifier
	Clock    testutil.Clock

	// Location is the zone whose calendar days define completeness. Required:
	// in UTC no day is ever 46 or 50 slots, so a missing zone would make DST
	// completeness silently wrong twice a year and look correct in between.
	Location *time.Location

	// TariffCode is the tariff to collect. Validated at construction.
	TariffCode string

	// VATRate is the rate the validation gate checks the inc/exc relationship
	// against. Taken literally — 0 is a legal VAT rate.
	VATRate float64

	Logger *slog.Logger
}

// Collector syncs one tariff's prices into the archive.
//
// It holds no accumulated state that matters. The counters are observability
// only, and everything that drives a decision — what we hold, whether a day is
// complete — is derived from the archive on each call. So a restart costs at
// most one extra sync, which is free because writes are idempotent.
type Collector struct {
	fetcher  RateFetcher
	store    prices.Store
	notifier notify.Notifier
	clock    testutil.Clock
	loc      *time.Location
	tariff   octopus.TariffCode
	vatRate  float64
	log      *slog.Logger

	mu     sync.Mutex
	status Status
}

// Status is the collector's health, as rendered on /healthz.
type Status struct {
	TariffCode string

	// LastAttempt moves on every sync; LastSuccess only on one that worked.
	// Keeping them apart is the point: equal values mean healthy, a LastSuccess
	// lagging LastAttempt means we are failing right now.
	LastAttempt time.Time
	LastSuccess time.Time
	LastError   string

	// KnownThrough is the end of the newest slot held. CompleteThrough is the
	// end of the newest fully populated local day. They differ, and the
	// difference is load-bearing: a publication can advance the horizon across a
	// whole day while leaving that day short of slots, so answering "do we have
	// tomorrow's prices?" with KnownThrough alone would say yes when it is no.
	KnownThrough    time.Time
	CompleteThrough time.Time

	// Cumulative counters, for /metrics.
	Syncs    int
	Failures int
	Inserted int
	Restated int
	Rejected int
	Warnings int
}

// SyncResult describes one sync.
type SyncResult struct {
	Stored   prices.PutResult
	Rejected []prices.Rejection
	Warnings []prices.Warning

	// UpToDate is true when the supplier's horizon had not moved, so no rates
	// were fetched at all.
	UpToDate bool

	// TomorrowComplete reports whether the next local day holds every slot it
	// should. KeepPolling is the scheduling consequence: the publication still
	// looks like it is landing, so come back soon rather than alerting.
	TomorrowComplete bool
	KeepPolling      bool
}

// New validates options and builds a Collector.
//
// It refuses rather than defaulting, because the failure it is avoiding is a
// collector that starts and quietly does nothing: the archive stops filling and
// there is nothing for /healthz to complain about.
func New(opts Options) (*Collector, error) {
	if opts.Fetcher == nil {
		return nil, fmt.Errorf("collector: a Fetcher is required")
	}
	if opts.Store == nil {
		return nil, fmt.Errorf("collector: a Store is required")
	}
	if opts.Location == nil {
		return nil, fmt.Errorf("collector: a Location is required; " +
			"completeness is a local-calendar question and in UTC no day is 46 or 50 slots")
	}
	if opts.TariffCode == "" {
		return nil, fmt.Errorf("collector: a TariffCode is required")
	}
	tariff, err := octopus.ParseTariffCode(opts.TariffCode)
	if err != nil {
		return nil, fmt.Errorf("collector: invalid TariffCode: %w", err)
	}

	c := &Collector{
		fetcher:  opts.Fetcher,
		store:    opts.Store,
		notifier: opts.Notifier,
		clock:    opts.Clock,
		loc:      opts.Location,
		tariff:   tariff,
		vatRate:  opts.VATRate,
		log:      opts.Logger,
		status:   Status{TariffCode: opts.TariffCode},
	}
	if c.notifier == nil {
		c.notifier = notify.Nop{}
	}
	if c.clock == nil {
		c.clock = testutil.RealClock{}
	}
	if c.log == nil {
		c.log = slog.Default()
	}
	return c, nil
}

// Status returns a snapshot of health.
func (c *Collector) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.status
}

// Sync brings the archive up to date, and assesses completeness.
//
// The sequence is deliberately cheapest-first. The horizon probe is one small
// request; when it shows nothing new — which is the common case, since the watch
// runs every few minutes and the supplier publishes once a day — no rates are
// fetched at all. Re-downloading a day on every tick would be gratuitous load on
// a public API that rate-limits.
func (c *Collector) Sync(ctx context.Context) (SyncResult, error) {
	now := c.clock.Now()
	c.markAttempt(now)

	horizon, err := c.fetcher.Horizon(ctx, c.tariff)
	if err != nil {
		return SyncResult{}, c.fail(fmt.Errorf("collector: horizon probe: %w", err))
	}

	known, err := c.store.KnownThrough(ctx, c.tariff.Code)
	if err != nil {
		return SyncResult{}, c.fail(fmt.Errorf("collector: read archive horizon: %w", err))
	}

	var res SyncResult
	switch {
	case horizon.IsZero():
		// The supplier has published nothing. Not an error — a newly launched
		// product legitimately has none — but it is assessed below, because if
		// it is also true for TODAY then energy is being consumed that cannot
		// be priced.
		res.UpToDate = true
	case !known.IsZero() && !horizon.After(known):
		res.UpToDate = true
	default:
		from := known.Add(-fetchOverlap)
		if known.IsZero() {
			from = time.Time{} // unbounded: let the supplier's history decide
		}
		fetched, err := c.fetchRange(ctx, from, horizon)
		if err != nil {
			return SyncResult{}, c.fail(err)
		}
		res = fetched
	}

	// Re-read rather than trusting `horizon`: what we can answer for is what the
	// archive actually holds, which is not the same thing as what the supplier
	// claims to have published. A slot rejected by validation would otherwise
	// inflate this.
	if held, err := c.store.KnownThrough(ctx, c.tariff.Code); err == nil {
		c.setKnownThrough(held)
	}

	c.assess(ctx, now, &res)
	c.markSuccess(now, res)
	return res, nil
}

// Backfill fetches an explicit range, in bounded chunks.
//
// Used for the initial load of a product's history and for the catch-up sweep.
// Chunked so a first run does not depend on one enormous response and so a
// failure part-way through costs only the current chunk — everything already
// written stays written, which is safe precisely because writes are idempotent.
func (c *Collector) Backfill(ctx context.Context, from, to time.Time) (SyncResult, error) {
	now := c.clock.Now()
	c.markAttempt(now)

	var total SyncResult
	for start := from; start.Before(to); start = start.Add(backfillChunk) {
		end := start.Add(backfillChunk)
		if end.After(to) {
			end = to
		}
		chunk, err := c.fetchRange(ctx, start, end)
		if err != nil {
			return total, c.fail(err)
		}
		total.Stored.Inserted += chunk.Stored.Inserted
		total.Stored.Unchanged += chunk.Stored.Unchanged
		total.Stored.Restated += chunk.Stored.Restated
		total.Rejected = append(total.Rejected, chunk.Rejected...)
		total.Warnings = append(total.Warnings, chunk.Warnings...)
	}

	// A sweep is the path a restatement is most likely to be found on — it is the
	// only thing that re-reads days we already hold — so it must report one.
	c.assess(ctx, now, &total)
	c.markSuccess(now, total)
	return total, nil
}

// CatchUp re-reads the last n days so an outage heals itself.
//
// It is expected to find nothing, and finding nothing is free: every slot is
// already held, so the store reports them unchanged and no notification fires.
// That is what makes it safe to run on a schedule and to overlap with the watch.
func (c *Collector) CatchUp(ctx context.Context, days int) (SyncResult, error) {
	if days <= 0 {
		days = 7
	}
	now := c.clock.Now()
	start, _ := localDayWindow(now.AddDate(0, 0, -days), c.loc)
	_, end := localDayWindow(now, c.loc)
	return c.Backfill(ctx, start, end)
}

// fetchRange fetches, validates and stores one range.
func (c *Collector) fetchRange(ctx context.Context, from, to time.Time) (SyncResult, error) {
	rates, err := c.fetcher.UnitRates(ctx, c.tariff, from, to)
	if err != nil {
		return SyncResult{}, fmt.Errorf("collector: fetch unit rates: %w", err)
	}
	if len(rates) == 0 {
		return SyncResult{}, nil
	}

	slots := prices.FromRates(c.tariff.Code, rates, c.clock.Now())
	v := prices.Validate(slots, prices.ValidateOptions{
		TariffCode: c.tariff.Code,
		VATRate:    c.vatRate,
	})

	var res SyncResult
	res.Rejected = v.Rejected
	res.Warnings = v.Warnings

	// Only validated slots are stored. A rejected one is never written and never
	// dropped in silence: it is reported, counted and alerted on, because a
	// quietly discarded slot becomes unpriced energy weeks later and by then
	// nobody can tell whether the price was missing upstream or we ate it.
	if len(v.Accepted) > 0 {
		stored, err := c.store.Put(ctx, v.Accepted)
		if err != nil {
			return SyncResult{}, fmt.Errorf("collector: store slots: %w", err)
		}
		res.Stored = stored
	}
	return res, nil
}

// assess decides what the archive's state means, and whether to tell anybody.
//
// Three conditions, in descending order of how bad they are. Only one alert is
// raised per sync, because the worst of them subsumes the others: being told
// "tomorrow is two slots short" while today is entirely unpriced would bury the
// thing that actually matters.
func (c *Collector) assess(ctx context.Context, now time.Time, res *SyncResult) {
	// Data-quality events come first and unconditionally. They describe what we
	// just received rather than what we are still waiting for, so they are NOT
	// subject to the "worst condition wins" rule below — an incomplete day must
	// not swallow the news that a stored price was revised.
	if n := len(res.Rejected); n > 0 {
		c.alert(ctx, notify.Event{
			Kind: notify.KindValidationRejected, Severity: notify.SeverityWarn,
			Summary:  fmt.Sprintf("%d price slots failed validation and were not stored", n),
			DedupKey: c.tariff.Code,
			Detail: map[string]any{
				"tariff_code": c.tariff.Code,
				"count":       n,
				"reasons":     rejectionReasons(res.Rejected),
			},
		})
	}
	if res.Stored.Restated > 0 {
		c.alert(ctx, notify.Event{
			Kind: notify.KindRestatement, Severity: notify.SeverityError,
			Summary: fmt.Sprintf("%d prices were revised after we had already stored them", res.Stored.Restated),
			// No dedup key: each restatement is its own fact, and they are rare
			// enough that suppressing one would be the greater risk.
			Detail: map[string]any{"tariff_code": c.tariff.Code, "count": res.Stored.Restated},
		})
	}

	todayStart, todayEnd := localDayWindow(now, c.loc)
	tomorrow := now.AddDate(0, 0, 1)
	tomorrowStart, tomorrowEnd := localDayWindow(tomorrow, c.loc)

	today := c.dayCompleteness(ctx, now, todayStart, todayEnd)
	tmrw := c.dayCompleteness(ctx, tomorrow, tomorrowStart, tomorrowEnd)

	res.TomorrowComplete = tmrw.Complete
	c.recordCompleteThrough(today, tmrw, todayEnd, tomorrowEnd)

	inWatch := c.inWatchWindow(now)
	pastDeadline := c.pastDeadline(now)

	// Worst first: energy is being consumed right now that cannot be priced.
	if today.Present == 0 {
		c.alert(ctx, notify.Event{
			Kind: notify.KindPricesMissing, Severity: notify.SeverityError,
			Summary:  "no prices held for today; energy consumed now cannot be priced",
			DedupKey: todayStart.Format(time.RFC3339),
			Detail: map[string]any{
				"tariff_code": c.tariff.Code,
				"day":         todayStart.In(c.loc).Format("2006-01-02"),
				"expected":    today.Expected,
			},
		})
		return
	}
	if !today.Complete {
		c.alert(ctx, notify.Event{
			Kind: notify.KindPricesMissing, Severity: notify.SeverityError,
			Summary:  "today's prices are incomplete",
			DedupKey: todayStart.Format(time.RFC3339),
			Detail: map[string]any{
				"tariff_code": c.tariff.Code,
				"day":         todayStart.In(c.loc).Format("2006-01-02"),
				"present":     today.Present, "expected": today.Expected,
				"missing": len(today.Missing),
			},
		})
		return
	}

	// Tomorrow. Before the publication window it does not exist yet, which is the
	// normal state for most of the day and must never alert. Inside the window an
	// incomplete day means the publication is still landing — keep polling. Past
	// the deadline it has stopped being a wait and become a gap.
	switch {
	case tmrw.Complete:
		c.resolve(notify.KindPricesMissing, tomorrowStart.Format(time.RFC3339))
		c.resolve(notify.KindDayIncomplete, tomorrowStart.Format(time.RFC3339))
	case pastDeadline:
		c.alert(ctx, notify.Event{
			Kind: notify.KindDayIncomplete, Severity: notify.SeverityError,
			Summary:  "tomorrow's prices are still incomplete past the publication deadline",
			DedupKey: tomorrowStart.Format(time.RFC3339),
			Detail: map[string]any{
				"tariff_code": c.tariff.Code,
				"day":         tomorrowStart.In(c.loc).Format("2006-01-02"),
				"present":     tmrw.Present, "expected": tmrw.Expected,
				"missing_tail_only": tmrw.MissingTailOnly(),
			},
		})
	case inWatch:
		res.KeepPolling = true
	}

}

// dayCompleteness reads a day out of the archive and checks it.
func (c *Collector) dayCompleteness(ctx context.Context, day, start, end time.Time) prices.DayCompleteness {
	held, err := c.store.Range(ctx, c.tariff.Code, start, end)
	if err != nil {
		// A read failure is not a completeness verdict. Report an empty day and
		// let the error surface through the caller rather than asserting a day
		// is complete on no evidence.
		c.log.WarnContext(ctx, "collector: could not read day from archive",
			"error", err, "day", start.Format(time.RFC3339))
		return prices.DayCompleteness{Start: start, End: end}
	}
	return prices.CheckDay(held, day, c.loc)
}

// recordCompleteThrough stores the end of the newest fully populated day.
//
// It stops at the first incomplete day rather than taking the newest complete
// one, because the question this answers is "how far can we price without
// gaps?" — and a complete tomorrow behind an incomplete today would not make
// today priceable.
func (c *Collector) recordCompleteThrough(today, tomorrow prices.DayCompleteness, todayEnd, tomorrowEnd time.Time) {
	var through time.Time
	if today.Complete {
		through = todayEnd
		if tomorrow.Complete {
			through = tomorrowEnd
		}
	}
	c.mu.Lock()
	c.status.CompleteThrough = through
	c.mu.Unlock()
}

// inWatchWindow reports whether we are in the part of the day when a publication
// is expected.
func (c *Collector) inWatchWindow(now time.Time) bool {
	h := now.In(c.loc).Hour()
	return h >= watchFromLocalHour && h < watchDeadlineLocalHour
}

// pastDeadline reports whether waiting for tomorrow has stopped being reasonable.
func (c *Collector) pastDeadline(now time.Time) bool {
	return now.In(c.loc).Hour() >= watchDeadlineLocalHour
}

// DueIn says how long until the next sync is worth doing.
//
// A pure function of the clock and the last assessment, so the schedule can be
// asserted in a test without running a loop. Short while actively waiting for a
// publication, long otherwise — the idle cadence exists only as a backstop
// against a publication that somehow misses the watch window.
func (c *Collector) DueIn() time.Duration {
	now := c.clock.Now()
	c.mu.Lock()
	completeThrough := c.status.CompleteThrough
	c.mu.Unlock()

	_, tomorrowEnd := localDayWindow(now.AddDate(0, 0, 1), c.loc)
	tomorrowCovered := !completeThrough.Before(tomorrowEnd)

	if c.inWatchWindow(now) && !tomorrowCovered {
		return watchInterval
	}
	return idleInterval
}

// Run syncs on the schedule DueIn describes until ctx is cancelled.
//
// Deliberately thin: all the policy is in Sync and DueIn, which are testable
// without it. A sync failure is logged and the loop continues — fail-open means
// the collector keeps trying rather than exiting and leaving the archive frozen.
func (c *Collector) Run(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	// A sweep on startup, so a redeploy heals anything missed while it was down.
	if _, err := c.CatchUp(ctx, 7); err != nil {
		c.log.WarnContext(ctx, "collector: startup sweep failed", "error", err)
	}

	for {
		if ctx.Err() != nil {
			return
		}
		if _, err := c.Sync(ctx); err != nil {
			c.log.WarnContext(ctx, "collector: sync failed, keeping the archive as it was", "error", err)
		}

		timer := time.NewTimer(c.DueIn())
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// ---------------------------------------------------------------------------
// bookkeeping
// ---------------------------------------------------------------------------

func (c *Collector) markAttempt(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.status.LastAttempt = now
	c.status.Syncs++
}

// fail records an error and returns it unchanged, so a caller reads the original.
func (c *Collector) fail(err error) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.status.LastError = err.Error()
	c.status.Failures++
	return err
}

// markSuccess clears the last error and folds in the sync's counters.
//
// Clearing matters: without it /healthz would stay red after the problem had
// gone, and an indicator that does not recover stops being read.
func (c *Collector) markSuccess(now time.Time, res SyncResult) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.status.LastSuccess = now
	c.status.LastError = ""
	c.status.Inserted += res.Stored.Inserted
	c.status.Restated += res.Stored.Restated
	c.status.Rejected += len(res.Rejected)
	c.status.Warnings += len(res.Warnings)
}

// setKnownThrough is called after a successful assess to publish the horizon.
func (c *Collector) setKnownThrough(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.status.KnownThrough = t
}

func (c *Collector) alert(ctx context.Context, e notify.Event) {
	if err := c.notifier.Notify(ctx, e); err != nil {
		// Fail-open: a notification that did not land must never stop the
		// collector. Failing to archive prices because an alerting endpoint was
		// down would be worse than the condition being reported.
		c.log.WarnContext(ctx, "collector: could not deliver notification",
			"error", err, "kind", e.Kind)
	}
}

// resolve clears a condition so its next occurrence alerts immediately rather
// than waiting out the throttle's cooldown.
func (c *Collector) resolve(kind, dedupKey string) {
	if r, ok := c.notifier.(interface{ Resolve(string, string) }); ok {
		r.Resolve(kind, dedupKey)
	}
}

// rejectionReasons summarises a batch of rejections for a log line or an alert,
// so the detail is a handful of reasons rather than every offending slot.
func rejectionReasons(rejections []prices.Rejection) string {
	counts := map[prices.RejectReason]int{}
	for _, r := range rejections {
		counts[r.Reason]++
	}
	parts := make([]string, 0, len(counts))
	for reason, n := range counts {
		parts = append(parts, fmt.Sprintf("%s=%d", reason, n))
	}
	return strings.Join(parts, " ")
}

// localDayWindow returns the UTC bounds of the local day containing t.
//
// The next midnight comes from the calendar rather than from adding 24h, so the
// zone decides how long the day was — 23, 24 or 25 hours.
func localDayWindow(t time.Time, loc *time.Location) (start, end time.Time) {
	local := t.In(loc)
	start = time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
	end = time.Date(local.Year(), local.Month(), local.Day()+1, 0, 0, 0, 0, loc)
	return start.UTC(), end.UTC()
}
