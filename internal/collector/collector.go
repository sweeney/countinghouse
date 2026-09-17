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
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"sort"
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

	// publishedTailSlack is how many trailing half hours the FURTHEST published day
	// may be short by before it counts as a gap rather than as the supplier's horizon.
	//
	// MEASURED, from 729 local days of archived prices plus a direct probe of the live
	// API on 2026-09-13: 724 of 729 days hold a full local day (48 slots, or 46/50
	// across a DST changeover), and the only incomplete day is always the furthest
	// published one, short by exactly its last two half hours. The horizon ends at
	// 23:00 local, so a 48-slot BST day loses two; the same holds in GMT, where the day
	// ends at 00:00Z against a 23:00Z horizon.
	//
	// Publication time — after 16:00 daily — is documented by the supplier and by third
	// parties. The 23:00 end-of-horizon is NOT publicly documented and rests on the
	// measurement above, which is one of the reasons to keep the archive.
	//
	// Two, not "any tail": a day short by thirty half hours is also tail-only, and that
	// one means the publication barely landed and IS worth saying past the deadline.
	publishedTailSlack = 2

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

	// VATRateAt resolves the expected VAT rate for one slot from its valid_from,
	// returning false when no agreement covers it. Takes precedence over VATRate.
	//
	// A function rather than a number because a scalar was wrong three ways: it was
	// frozen at whatever was in force when the process started, it came from one
	// instant rather than from the agreement owning each slot, and an unresolvable
	// rate silently became 0 — which, while the check was a Gate A rejection, meant
	// 100% of slots rejected for vat_mismatch and the archive quietly ceasing to fill
	// while /healthz showed fetches succeeding.
	VATRateAt func(time.Time) (float64, bool)

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
	fetcher   RateFetcher
	store     prices.Store
	notifier  notify.Notifier
	clock     testutil.Clock
	loc       *time.Location
	tariff    octopus.TariffCode
	vatRate   float64
	vatRateAt func(time.Time) (float64, bool)
	log       *slog.Logger

	mu        sync.Mutex
	status    Status
	lastSweep time.Time
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

	// LastErrorClass is LastError reduced to one of a fixed set of causes, derived
	// HERE because this is where the typed error still exists.
	//
	// /healthz is unauthenticated and publishes the class; /metrics, behind auth,
	// publishes the text. Deriving the class downstream from the string cannot work:
	// every call site wraps before storing, so the stored value always begins
	// "collector: <stage>: " and an octopus-anchored match found none of it. It is
	// also simply worse — the status code is right here on the error, rather than
	// something to recover by parsing prose that embeds an untrusted response body.
	LastErrorClass string

	// KnownTo is the end of the newest slot held. CompleteTo is the
	// end of the newest fully populated local day. They differ, and the
	// difference is load-bearing: a publication can advance the horizon across a
	// whole day while leaving that day short of slots, so answering "do we have
	// tomorrow's prices?" with KnownTo alone would say yes when it is no.
	KnownTo    time.Time
	CompleteTo time.Time

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
		fetcher:   opts.Fetcher,
		store:     opts.Store,
		notifier:  opts.Notifier,
		clock:     opts.Clock,
		loc:       opts.Location,
		tariff:    tariff,
		vatRate:   opts.VATRate,
		vatRateAt: opts.VATRateAt,
		log:       opts.Logger,
		status:    Status{TariffCode: opts.TariffCode},
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

	known, err := c.store.KnownTo(ctx, c.tariff.Code)
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
		from, to := known.Add(-fetchOverlap), horizon
		if known.IsZero() {
			// Nothing held: ask for EVERYTHING rather than for [unbounded, horizon).
			// Both bounds go, not just the lower one — an upper bound without a
			// lower one is not a well-formed request (the supplier rejects it), and
			// it would be meaningless here anyway, since the horizon is by
			// definition the newest thing the supplier has.
			from, to = time.Time{}, time.Time{}
		}
		fetched, err := c.fetchRange(ctx, from, to)
		if err != nil {
			return SyncResult{}, c.fail(err)
		}
		res = fetched
	}

	// Re-read rather than trusting `horizon`: what we can answer for is what the
	// archive actually holds, which is not the same thing as what the supplier
	// claims to have published. A slot rejected by validation would otherwise
	// inflate this.
	if held, err := c.store.KnownTo(ctx, c.tariff.Code); err == nil {
		c.setKnownTo(held)
	}

	// A sync that stored prices and then could not READ the archive is not a
	// success. Reporting one cleared LastError and left CompleteTo stale, so
	// /healthz answered ok for an archive it cannot read — the same failure
	// dayCompleteness was fixed for, dropped one frame up.
	if err := c.assess(ctx, now, &res); err != nil {
		return res, c.fail(err)
	}
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
	start, _ := prices.LocalDayWindow(now.AddDate(0, 0, -days), c.loc)
	_, end := prices.LocalDayWindow(now, c.loc)
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
		TariffCode:      c.tariff.Code,
		VATRate:         c.vatRate,
		ExpectVATRateAt: c.vatRateAt,
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
// It returns an error ONLY for a state that makes the verdict meaningless — an
// unreadable archive. Everything else it handles by alerting, because those are
// findings about the data rather than failures of the sync.
func (c *Collector) assess(ctx context.Context, now time.Time, res *SyncResult) error {
	// Data-quality events come first and unconditionally. They describe what we
	// just received rather than what we are still waiting for, so they are NOT
	// subject to the "worst condition wins" rule below — an incomplete day must
	// not swallow the news that a stored price was revised.
	// A whole batch implying ONE consistent VAT rate that is not the configured
	// one is a different animal from a stray slot: it is a rate change, and it is
	// the operator's cue to edit config. Raised before the rejection alert because
	// it explains a class of warning rather than reporting a loss.
	if drift, ok := vatDrift(res.Warnings, c.vatRate); ok {
		c.alert(ctx, notify.Event{
			Kind: notify.KindAgreementDrift, Severity: notify.SeverityError,
			Summary: fmt.Sprintf(
				"the supplier is charging VAT at %.2f%%, but configuration says %.2f%%",
				drift.Implied*100, drift.Configured*100),
			DedupKey: c.tariff.Code,
			Detail: map[string]any{
				"tariff_code":         c.tariff.Code,
				"implied_vat_rate":    drift.Implied,
				"configured_vat_rate": drift.Configured,
				"slots":               drift.Slots,
				"what_to_do": "set vat_rate on the agreement block covering these dates; " +
					"prices and standing charges are still being archived correctly meanwhile, " +
					"because both are stored as the supplier's own inc-VAT figures",
			},
		})
	}

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

	todayStart, todayEnd := prices.LocalDayWindow(now, c.loc)
	tomorrow := now.AddDate(0, 0, 1)
	tomorrowStart, tomorrowEnd := prices.LocalDayWindow(tomorrow, c.loc)

	today, todayErr := c.dayCompleteness(ctx, now, todayStart, todayEnd)
	tmrw, tmrwErr := c.dayCompleteness(ctx, tomorrow, tomorrowStart, tomorrowEnd)

	// An unreadable archive is its own condition, and a loud one: we cannot say whether
	// anything is missing, so every completeness verdict below would be a guess dressed
	// as a fact. Reported and returned, rather than folded into "no prices held".
	if todayErr != nil || tmrwErr != nil {
		err := todayErr
		if err == nil {
			err = tmrwErr
		}
		c.alert(ctx, notify.Event{
			Kind: notify.KindArchiveUnreadable, Severity: notify.SeverityError,
			Summary:  "the price archive could not be read; completeness is unknown",
			DedupKey: c.tariff.Code,
			Detail: map[string]any{
				"tariff_code": c.tariff.Code,
				"error":       err.Error(),
			},
		})
		return fmt.Errorf("collector: read archive for completeness: %w", err)
	}
	c.resolve(notify.KindArchiveUnreadable, c.tariff.Code)

	res.TomorrowComplete = tmrw.Complete
	c.recordCompleteTo(today, tmrw, todayEnd, tomorrowEnd)

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
		return nil
	}
	// A routine tail gap on today is the normal state for most of the day: the
	// supplier's horizon ends at 23:00 local, so until the ~16:00 publication moves it,
	// today is short by exactly its final two half hours. Alerting fires an ERROR every
	// morning on a healthy feed. See publishedTailSlack for the measurement.
	//
	// It still costs something real — the last hour of today cannot be priced yet — but
	// that is visible as the difference between known_to and complete_to, which is what
	// those two fields are for. It is not a page.
	routineTail := today.MissingTailOnly() && len(today.Missing) <= publishedTailSlack
	if routineTail {
		c.resolve(notify.KindPricesMissing, todayStart.Format(time.RFC3339))
	}
	if !today.Complete && !routineTail {
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
		return nil
	}
	// Note what changed here: with a routine tail gap we now fall THROUGH to the
	// tomorrow branch instead of returning. Previously an incomplete today short-
	// circuited assess, so for the ~16 hours a day that today was tail-short, the
	// tomorrow branch never ran — meaning the publication watch was never confirmed by
	// the path written to confirm it.

	// Tomorrow. Before the publication window it does not exist yet, which is the
	// normal state for most of the day and must never alert. Inside the window an
	// incomplete day means the publication is still landing — keep polling. Past
	// the deadline it has stopped being a wait and become a gap.
	switch {
	case tmrw.Complete:
		c.resolve(notify.KindPricesMissing, tomorrowStart.Format(time.RFC3339))
		c.resolve(notify.KindDayIncomplete, tomorrowStart.Format(time.RFC3339))
	case tmrw.MissingTailOnly() && len(tmrw.Missing) <= publishedTailSlack:
		// The supplier's published horizon stops short of the furthest day's end, so a
		// small TAIL gap on tomorrow is what a healthy feed looks like — not a fault,
		// and not something to escalate at the deadline. See publishedTailSlack for
		// the measurement.
		//
		// It is resolved rather than merely skipped: if an earlier, larger gap alerted
		// while the publication was still landing, this is the point at which it has
		// landed as fully as it ever will.
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
	return nil
}

// dayCompleteness reads a day out of the archive and checks it.
func (c *Collector) dayCompleteness(ctx context.Context, day, start, end time.Time) (prices.DayCompleteness, error) {
	held, err := c.store.Range(ctx, c.tariff.Code, start, end)
	if err != nil {
		// A read failure is not a completeness verdict, and returning an empty day made
		// it one: Present == 0 is indistinguishable from "no prices held", so assess
		// paged "no prices held for today; energy consumed now cannot be priced" about
		// an archive that may be perfectly full. Failure reading as data, which is the
		// thing this package is otherwise careful about.
		//
		// The error now reaches the caller, which reports the archive being unreadable
		// as its own condition — genuinely alert-worthy, and a different thing to say.
		c.log.WarnContext(ctx, "collector: could not read day from archive",
			"error", err, "day", start.Format(time.RFC3339))
		return prices.DayCompleteness{Start: start, End: end}, err
	}
	return prices.CheckDay(held, day, c.loc), nil
}

// recordCompleteTo stores the end of the newest fully populated day.
//
// It stops at the first incomplete day rather than taking the newest complete
// one, because the question this answers is "how far can we price without
// gaps?" — and a complete tomorrow behind an incomplete today would not make
// today priceable.
func (c *Collector) recordCompleteTo(today, tomorrow prices.DayCompleteness, todayEnd, tomorrowEnd time.Time) {
	// "How far can we price without gaps?" — so it runs forward from today and stops
	// at the FIRST thing that would break a bill, which is an interior hole. The
	// supplier's routine two-slot tail on the furthest published day is not such a
	// thing: everything before it is priceable, and that is where this stops.
	var completeTo time.Time
	switch {
	case today.Complete:
		completeTo = todayEnd
		switch {
		case tomorrow.Complete:
			completeTo = tomorrowEnd
		case tomorrow.MissingTailOnly() && len(tomorrow.Missing) <= publishedTailSlack:
			// After the afternoon publication TOMORROW is the furthest day and carries
			// the tail, so this is the routine state for the rest of the day.
			completeTo = tomorrow.TailGapStart()
		}
	case today.MissingTailOnly() && len(today.Missing) <= publishedTailSlack:
		// Before the publication, TODAY is the furthest day and carries the tail.
		// Leaving the ZERO time here was read downstream as "no complete day of prices
		// held" and degraded /healthz for the ~16 hours a day before the publication,
		// on an archive holding years of prices.
		completeTo = today.TailGapStart()
	}
	// This does make complete_to equal known_to in the routine case, which was once the
	// argument against advancing it. That turns out to be the point rather than the
	// objection: the two then diverge precisely when an INTERIOR hole exists, which is
	// the fault worth alerting on. A field permanently a day behind its neighbour
	// carries less information than one that matches it until something is wrong.
	c.mu.Lock()
	c.status.CompleteTo = completeTo
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
// sweepAtLocalHour is the local hour the daily re-read runs at: quiet, and after the
// publication window has closed so it never competes with the watch.
const sweepAtLocalHour = 2

// sweepDays is how far back a sweep re-reads. A restatement is most likely to land on a
// recent day, and a fortnight is cheap because every unchanged slot comes back Unchanged.
const sweepDays = 14

// sweepIfDue re-reads recent days once a day, so a restatement is actually FOUND.
//
// Sync short-circuits on UpToDate as soon as the horizon stops moving, which is the
// common case by design — so no day already held was ever re-read except within the 2h
// fetchOverlap. Backfill's own doc comment says a sweep "is the only thing that re-reads
// days we already hold", and nothing scheduled one, which meant restatement detection
// effectively only happened on restart. The data-model doc makes restatement the reason
// the archive is append-only with a retrieved_at and a log table, so the gap between
// stated intent and implementation was worth closing.
//
// Finding nothing is free: every slot comes back Unchanged.
func (c *Collector) sweepIfDue(ctx context.Context) {
	now := c.clock.Now().In(c.loc)
	if now.Hour() != sweepAtLocalHour {
		return
	}
	c.mu.Lock()
	last := c.lastSweep
	c.mu.Unlock()
	// Once per local day, not once per tick within the hour.
	if !last.IsZero() && last.In(c.loc).Format("2006-01-02") == now.Format("2006-01-02") {
		return
	}

	// Standing charges ride the sweep: once a day is ample for a figure that moves
	// about once a year, and it means a fresh deployment holds them within a day
	// rather than only after the next annual change.
	if sc, err := c.SyncStandingCharges(ctx); err != nil {
		c.log.WarnContext(ctx, "collector: standing-charge sync failed", "error", err)
	} else if sc.Inserted > 0 || sc.Restated > 0 {
		c.log.InfoContext(ctx, "collector: standing charges updated",
			"inserted", sc.Inserted, "restated", sc.Restated, "unchanged", sc.Unchanged)
	}

	res, err := c.CatchUp(ctx, sweepDays)
	c.mu.Lock()
	c.lastSweep = c.clock.Now()
	c.mu.Unlock()
	if err != nil {
		c.log.WarnContext(ctx, "collector: daily sweep failed", "error", err)
		return
	}
	// `unchanged` is reported too, and it is the number that matters most here: without
	// it the line reads "restated=0 inserted=0", which looks like the sweep did nothing
	// when it in fact re-read and rewrote several hundred rows. That difference stayed
	// invisible until the archive was inspected by hand the morning after the first run.
	c.log.InfoContext(ctx, "collector: daily sweep complete",
		"days", sweepDays,
		"unchanged", res.Stored.Unchanged,
		"restated", res.Stored.Restated,
		"inserted", res.Stored.Inserted)
}

func (c *Collector) DueIn() time.Duration {
	now := c.clock.Now()
	c.mu.Lock()
	completeThrough := c.status.CompleteTo
	c.mu.Unlock()

	_, tomorrowEnd := prices.LocalDayWindow(now.AddDate(0, 0, 1), c.loc)
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
		c.sweepIfDue(ctx)

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
	c.status.LastErrorClass = classify(err)
	c.status.Failures++
	return err
}

// classify reduces an error to one of a fixed set of causes, safe to publish on an
// unauthenticated endpoint.
//
// From the TYPE, not from the message: *octopus.APIError carries the status code,
// so a response body full of misleading digits cannot reach the decision.
func classify(err error) string {
	if err == nil {
		return ""
	}
	var apiErr *octopus.APIError
	if errors.As(err, &apiErr) {
		switch code := apiErr.StatusCode; {
		case code == http.StatusTooManyRequests:
			return "upstream rate limited"
		case code == http.StatusUnauthorized, code == http.StatusForbidden:
			return "upstream rejected our request"
		case code >= 500:
			return "upstream unavailable"
		default:
			return "upstream error"
		}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "upstream timeout"
	}
	// No type to lean on for these: the archive wraps with fmt.Errorf and the
	// message is our own, not an upstream body.
	switch msg := err.Error(); {
	case strings.Contains(msg, "open archive"), strings.Contains(msg, "read archive"),
		strings.Contains(msg, "prices: "):
		return "archive error"
	case strings.Contains(msg, "context deadline exceeded"),
		strings.Contains(msg, "i/o timeout"):
		return "upstream timeout"
	default:
		return "error"
	}
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

// setKnownTo is called after a successful assess to publish the horizon.
func (c *Collector) setKnownTo(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.status.KnownTo = t
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
	reasons := make([]string, 0, len(counts))
	for reason := range counts {
		reasons = append(reasons, string(reason))
	}
	// Sorted, for the reason checkJumps already sorts its groups: a log line that
	// reorders itself run to run is a log line nobody can diff.
	sort.Strings(reasons)
	parts := make([]string, 0, len(reasons))
	for _, reason := range reasons {
		parts = append(parts, fmt.Sprintf("%s=%d", reason, counts[prices.RejectReason(reason)]))
	}
	return strings.Join(parts, " ")
}

// vatDriftMinSlots is how many slots must agree before a VAT mismatch is called
// a rate change rather than a data blip.
//
// One mismatching slot is noise — a rounding artefact, or a single malformed row.
// A consistent implied rate across several is the supplier applying a different
// rate from the one configured, which is a statutory event somebody has to act
// on. Six is a quarter of a publication's worth of half hours: high enough that
// no plausible per-slot glitch reaches it, low enough that a standing-charge
// batch (which is a handful of rows at most) still can.
const vatDriftMinSlots = 6

// vatDriftEpsilon is how close two implied rates must be to count as the same
// rate. Generous, because the inputs are pence rounded by the supplier: at a
// standing charge of 56.19p a single rounded penny moves the implied rate by
// more than a thousandth.
const vatDriftEpsilon = 5e-4

// vatDrift reports a systematic disagreement between the supplier's VAT and ours.
type vatDriftFinding struct {
	Implied    float64
	Configured float64
	Slots      int
}

// vatDrift looks for a consistent implied VAT rate across the batch's mismatch
// warnings.
//
// Consistency is the whole test. Slots disagreeing with configuration in
// DIFFERENT directions are a data problem and stay per-slot warnings; slots all
// implying the same new rate are a tax change, and that is worth waking somebody
// for — once, with the number to put in config.
func vatDrift(warnings []prices.Warning, configured float64) (vatDriftFinding, bool) {
	var implied []float64
	for _, w := range warnings {
		if w.Kind == prices.WarnVATMismatch && w.ImpliedVATRate != nil {
			implied = append(implied, *w.ImpliedVATRate)
		}
	}
	if len(implied) < vatDriftMinSlots {
		return vatDriftFinding{}, false
	}
	for _, v := range implied {
		if math.Abs(v-implied[0]) > vatDriftEpsilon {
			return vatDriftFinding{}, false // not one rate; a data problem, not a tax change
		}
	}
	return vatDriftFinding{Implied: implied[0], Configured: configured, Slots: len(implied)}, true
}

// StandingChargeFetcher is the optional half of a RateFetcher that can also read
// standing charges, and StandingChargeArchive the optional half of a Store that
// can keep them.
//
// Optional, and detected by type assertion, for two reasons. It keeps every
// existing test double valid without a no-op method apiece; and it models the
// real deployment state honestly — an instance without a standing-charge archive
// falls back to the configured rate, which is exactly what every instance did
// before this existed.
type StandingChargeFetcher interface {
	StandingCharges(ctx context.Context, tariff octopus.TariffCode) ([]octopus.Rate, error)
}

// StandingChargeArchive keeps the supplier's daily standing charges.
type StandingChargeArchive interface {
	PutStandingCharges(ctx context.Context, charges []prices.DailyCharge) (prices.PutResult, error)
}

// SyncStandingCharges fetches and archives the tariff's standing charges.
//
// Run on the daily sweep rather than on every poll, because a standing charge
// changes roughly annually while unit prices change every half hour. Polling it
// at the unit-price cadence would be ~288 requests a day to learn nothing.
//
// Archiving these is what demotes configuration's vat_rate from a billing input
// to a checkable expectation: the standing charge was the last number in a bill
// still grossed up from config, and therefore the last one a stale config could
// silently get wrong. See docs/octopus-price-data-model.md.
func (c *Collector) SyncStandingCharges(ctx context.Context) (prices.PutResult, error) {
	fetcher, ok := c.fetcher.(StandingChargeFetcher)
	if !ok {
		return prices.PutResult{}, nil
	}
	archive, ok := c.store.(StandingChargeArchive)
	if !ok {
		return prices.PutResult{}, nil
	}

	rates, err := fetcher.StandingCharges(ctx, c.tariff)
	if err != nil {
		return prices.PutResult{}, fmt.Errorf("collector: fetch standing charges: %w", err)
	}
	if len(rates) == 0 {
		return prices.PutResult{}, nil
	}

	charges := prices.DailyChargesFromRates(c.tariff.Code, rates, c.clock.Now())
	v := prices.ValidateStandingCharges(charges, prices.ValidateOptions{
		TariffCode:      c.tariff.Code,
		VATRate:         c.vatRate,
		ExpectVATRateAt: c.vatRateAt,
	})

	if n := len(v.Rejected); n > 0 {
		c.alert(ctx, notify.Event{
			Kind: notify.KindValidationRejected, Severity: notify.SeverityWarn,
			Summary:  fmt.Sprintf("%d standing charges failed validation and were not stored", n),
			DedupKey: c.tariff.Code + ":standing",
			Detail:   map[string]any{"tariff_code": c.tariff.Code, "count": n},
		})
	}
	if drift, ok := vatDrift(v.Warnings, c.vatRate); ok {
		c.alert(ctx, notify.Event{
			Kind: notify.KindAgreementDrift, Severity: notify.SeverityError,
			Summary: fmt.Sprintf(
				"the supplier's standing charge implies VAT at %.2f%%, but configuration says %.2f%%",
				drift.Implied*100, drift.Configured*100),
			DedupKey: c.tariff.Code + ":standing",
			Detail: map[string]any{
				"tariff_code":         c.tariff.Code,
				"implied_vat_rate":    drift.Implied,
				"configured_vat_rate": drift.Configured,
			},
		})
	}
	if len(v.Accepted) == 0 {
		return prices.PutResult{}, nil
	}

	stored, err := archive.PutStandingCharges(ctx, v.Accepted)
	if err != nil {
		return prices.PutResult{}, fmt.Errorf("collector: store standing charges: %w", err)
	}
	// A revised standing charge moves every bill it ever touched, so it is worth
	// more noise than a revised half hour, not less.
	if stored.Restated > 0 {
		c.alert(ctx, notify.Event{
			Kind: notify.KindRestatement, Severity: notify.SeverityError,
			Summary: fmt.Sprintf("%d standing charges were revised after we had already stored them",
				stored.Restated),
			DedupKey: c.tariff.Code + ":standing",
			Detail:   map[string]any{"tariff_code": c.tariff.Code, "restated": stored.Restated},
		})
	}
	return stored, nil
}
