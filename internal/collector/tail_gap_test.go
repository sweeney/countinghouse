package collector

import (
	"context"
	"testing"
	"time"

	"github.com/sweeney/countinghouse/internal/notify"
)

// ---------------------------------------------------------------------------
// The supplier's published horizon stops short of the furthest day's end.
//
// MEASURED, from 729 local days of real archived prices plus a direct probe of the
// live API on 2026-09-13:
//
//   - 724 of 729 days hold exactly a full day (48 slots, or 46/50 across a DST
//     changeover). Every historical day is complete.
//   - The ONLY incomplete day is the furthest published one, and it is short by
//     exactly its last two half hours.
//   - The live horizon ended at 23:00 local on day+1 while day+0 held all 48, and
//     `retrieved_at` shows tomorrow's 46 slots arriving in one ~16:00 publication.
//
// Corroborated externally for the publication TIME — tomorrow's rates are published
// daily after 16:00 (energy-stats.uk, octopus.energy/blog/agile-pricing-explained).
// The 23:00 end-of-horizon is NOT publicly documented; it rests on the measurement
// above, which is one reason the archive exists.
//
// So a tail-only gap on the furthest day is the NORMAL state of a healthy feed, and
// alerting on it past the publication deadline fires an ERROR every night forever. A
// signal that is always red is a signal nobody reads — the same reasoning the
// throttle is justified by, arriving at the alert itself rather than at its volume.
//
// An INTERIOR hole is a different thing entirely and must still alert: it means a
// slot the supplier did publish never reached the archive.
// ---------------------------------------------------------------------------

// The routine case: tomorrow short by its final two half hours, past the deadline.
// This must be silent.
func TestTailOnlyGapPastDeadlineDoesNotAlert(t *testing.T) {
	// The fetcher's horizon ends 22:00Z = 23:00 local on the 11th, which is exactly
	// what the live API publishes: local day 11 Sep runs to 23:00Z, so its last two
	// half hours are absent.
	f := newFakeFetcher(ts(t, "2026-09-09T23:00:00Z"), ts(t, "2026-09-11T22:00:00Z"))
	h := newHarness(t, ts(t, "2026-09-10T16:08:00Z"), f)
	ctx := context.Background()

	if _, err := h.c.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	// Past 23:00 local, where the old behaviour escalated to SeverityError.
	h.clock.Set(ts(t, "2026-09-10T22:30:00Z"))
	if _, err := h.c.Sync(ctx); err != nil {
		t.Fatal(err)
	}

	if h.noti.has(notify.KindDayIncomplete) {
		t.Errorf("alerted on a two-slot TAIL gap past the deadline: %v\n"+
			"this is the supplier's routine published horizon — 724 of 729 measured days "+
			"look exactly like this — so alerting here fires an ERROR every night on a "+
			"healthy feed", h.noti.kinds())
	}
}

// An INTERIOR hole past the deadline must still alert: a slot the supplier published
// is missing from the archive, which is a real fault and the reason this check exists.
func TestInteriorHolePastDeadlineStillAlerts(t *testing.T) {
	f := newFakeFetcher(ts(t, "2026-09-09T23:00:00Z"), ts(t, "2026-09-11T22:00:00Z"))
	// Punch a hole in the middle of tomorrow, on top of the routine tail shortfall.
	f.omit = map[time.Time]bool{
		ts(t, "2026-09-11T10:00:00Z"): true,
		ts(t, "2026-09-11T10:30:00Z"): true,
	}
	h := newHarness(t, ts(t, "2026-09-10T16:08:00Z"), f)
	ctx := context.Background()

	if _, err := h.c.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	h.clock.Set(ts(t, "2026-09-10T22:30:00Z"))
	if _, err := h.c.Sync(ctx); err != nil {
		t.Fatal(err)
	}

	if !h.noti.has(notify.KindDayIncomplete) {
		t.Errorf("an interior hole past the deadline did not alert: %v — a slot the "+
			"supplier published is missing, which is the fault this check is for",
			h.noti.kinds())
	}
}

// A tail gap far larger than the horizon shortfall is NOT routine: it means the
// publication barely landed, and past the deadline that is worth saying.
func TestLargeTailGapPastDeadlineStillAlerts(t *testing.T) {
	// Horizon stops at 06:00Z on the 11th, so tomorrow is missing ~34 half hours —
	// a tail, but nothing like the routine two.
	f := newFakeFetcher(ts(t, "2026-09-09T23:00:00Z"), ts(t, "2026-09-11T06:00:00Z"))
	h := newHarness(t, ts(t, "2026-09-10T16:08:00Z"), f)
	ctx := context.Background()

	if _, err := h.c.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	h.clock.Set(ts(t, "2026-09-10T22:30:00Z"))
	if _, err := h.c.Sync(ctx); err != nil {
		t.Fatal(err)
	}

	if !h.noti.has(notify.KindDayIncomplete) {
		t.Errorf("a 34-slot tail gap past the deadline did not alert: %v — tolerating the "+
			"routine two-slot shortfall must not tolerate a day that barely published",
			h.noti.kinds())
	}
}

// The case that actually bites, and the one I first saw and then wrongly dismissed:
// for the ~16 hours between local midnight and the ~16:00 publication, the furthest
// published day IS TODAY, so today is the one short by two.
//
// Measured on 2026-09-13: at 18:26 BST the horizon ended 23:00 BST on the 14th, and the
// horizon does not move again until the next publication — so at 09:00 on the 14th,
// the 14th (by then "today") still held 46 of 48. Probing after 16:00 shows today
// complete and hides this entirely, which is how I came to contradict myself.
func TestRoutineTailGapOnTodayDoesNotAlertOrVoidCompleteTo(t *testing.T) {
	// Published through 22:00Z on the 10th: local 2026-09-10 (23:00Z on the 9th to
	// 23:00Z on the 10th) is missing its final two half hours. Clock at 09:00 local,
	// well before the publication window.
	f := newFakeFetcher(ts(t, "2026-09-08T23:00:00Z"), ts(t, "2026-09-10T22:00:00Z"))
	h := newHarness(t, ts(t, "2026-09-10T08:00:00Z"), f)

	if _, err := h.c.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}

	// No page. This fired every morning before the fix.
	if h.noti.has(notify.KindPricesMissing) {
		t.Errorf("alerted that today's prices are incomplete: %v — today is short by the "+
			"supplier's routine two-slot horizon, which is what every healthy morning "+
			"looks like", h.noti.kinds())
	}

	// And complete_to must not be the zero time, which downstream reads as "no complete
	// day of prices held" — degrading /healthz on an archive holding years of prices,
	// with a reason that is flatly false.
	st := h.c.Status()
	if st.CompleteTo.IsZero() {
		t.Error("CompleteTo is the zero time; /healthz reads that as an empty archive")
	}
	// It reports how far we can actually price: up to the start of the missing tail.
	want := ts(t, "2026-09-10T22:00:00Z")
	if !st.CompleteTo.Equal(want) {
		t.Errorf("CompleteTo = %s, want %s (the start of today's unpublished tail)",
			st.CompleteTo, want)
	}
}

// An interior hole in TODAY is still a page: energy is being consumed now against a
// price we do not hold and cannot expect.
func TestInteriorHoleInTodayStillAlerts(t *testing.T) {
	f := newFakeFetcher(ts(t, "2026-09-08T23:00:00Z"), ts(t, "2026-09-10T22:00:00Z"))
	f.omit = map[time.Time]bool{ts(t, "2026-09-10T06:00:00Z"): true}
	h := newHarness(t, ts(t, "2026-09-10T08:00:00Z"), f)

	if _, err := h.c.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !h.noti.has(notify.KindPricesMissing) {
		t.Errorf("an interior hole in today did not alert: %v", h.noti.kinds())
	}
	// With a hole rather than a tail, there is no single "priceable to here".
	if !h.c.Status().CompleteTo.IsZero() {
		t.Errorf("CompleteTo = %s; an interior hole has no contiguous end",
			h.c.Status().CompleteTo)
	}
}

// With today tail-short, assess must still evaluate TOMORROW. It used to return early
// on any incomplete today — so for the ~16 hours a day that today was tail-short, the
// tomorrow branch never ran and the publication watch was never confirmed by the path
// written to confirm it.
func TestTodayTailGapStillEvaluatesTomorrow(t *testing.T) {
	// Today tail-short AND tomorrow entirely absent, inside the watch window.
	f := newFakeFetcher(ts(t, "2026-09-08T23:00:00Z"), ts(t, "2026-09-10T22:00:00Z"))
	h := newHarness(t, ts(t, "2026-09-10T15:10:00Z"), f)

	res, err := h.c.Sync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !res.KeepPolling {
		t.Error("tomorrow is absent inside the watch window, so the collector must keep " +
			"polling — an incomplete today must no longer short-circuit that")
	}
}

// A SMALL interior hole, with no tail gap at all, must still alert.
//
// This is the case that gives the tail-only check its teeth, and my first version of
// this file did not have it: the other interior-hole tests also carry the routine tail
// shortfall, so their gaps total three slots and exceed the slack — meaning they alert
// on the COUNT and would pass even if the tail-only condition were deleted. Mutating
// MissingTailOnly() away proved exactly that.
//
// Here the supplier has published the whole day, and one slot in the middle never
// reached us. One missing slot is within the slack, so only the tail-only condition can
// distinguish it from the routine horizon.
func TestSmallInteriorHoleWithNoTailGapStillAlerts(t *testing.T) {
	// Published through the full local day end (23:00Z), so there is NO tail gap.
	f := newFakeFetcher(ts(t, "2026-09-08T23:00:00Z"), ts(t, "2026-09-10T23:00:00Z"))
	f.omit = map[time.Time]bool{ts(t, "2026-09-10T06:00:00Z"): true}
	h := newHarness(t, ts(t, "2026-09-10T08:00:00Z"), f)

	if _, err := h.c.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !h.noti.has(notify.KindPricesMissing) {
		t.Errorf("a single interior hole did not alert: %v — one missing slot is within "+
			"the tail slack, so tolerating it by COUNT alone would swallow a real gap in "+
			"the middle of the day", h.noti.kinds())
	}
	if !h.c.Status().CompleteTo.IsZero() {
		t.Errorf("CompleteTo = %s; an interior hole has no contiguous end",
			h.c.Status().CompleteTo)
	}
}

// The mirror for tomorrow: one interior slot missing, no tail gap, past the deadline.
func TestSmallInteriorHoleInTomorrowStillAlerts(t *testing.T) {
	f := newFakeFetcher(ts(t, "2026-09-09T23:00:00Z"), ts(t, "2026-09-11T23:00:00Z"))
	f.omit = map[time.Time]bool{ts(t, "2026-09-11T09:00:00Z"): true}
	h := newHarness(t, ts(t, "2026-09-10T16:08:00Z"), f)
	ctx := context.Background()

	if _, err := h.c.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	h.clock.Set(ts(t, "2026-09-10T22:30:00Z"))
	if _, err := h.c.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if !h.noti.has(notify.KindDayIncomplete) {
		t.Errorf("a single interior hole in tomorrow did not alert past the deadline: %v",
			h.noti.kinds())
	}
}
