package energy

import (
	"fmt"
	"time"
)

// MaxBuckets caps how many buckets a single series response may carry. A finer
// interval over a long window can blow the response size up unboundedly; rather
// than silently degrade, ResolveInterval errors and asks the caller for a
// coarser interval. 1000 keeps a stacked-chart payload well-bounded (PLAN §A).
const MaxBuckets = 1000

// Interval is one allowed bucketing granularity.
//
// Token is the Flux duration literal passed to aggregateWindow(every:) (and to
// the influx series builders). Duration is the Go-side fixed length used to
// step the canonical axis and, via bucketHours, to energy-derive avg_w. Calendar
// marks day-and-larger intervals whose real length is NOT a fixed Duration: a
// London calendar day is 23h or 25h across a DST changeover, so the axis must
// step by calendar date (time.Date day+1) rather than by adding Duration.
type Interval struct {
	Token    string
	Duration time.Duration
	Calendar bool
}

// intervals is the allowed set, smallest first. Order matters: DefaultInterval
// and the coarser-fallback logic walk it ascending.
var intervals = []Interval{
	{Token: "5m", Duration: 5 * time.Minute},
	{Token: "15m", Duration: 15 * time.Minute},
	{Token: "30m", Duration: 30 * time.Minute},
	{Token: "1h", Duration: time.Hour},
	{Token: "6h", Duration: 6 * time.Hour},
	{Token: "1d", Duration: 24 * time.Hour, Calendar: true},
}

// intervalByToken indexes intervals by their Flux token.
var intervalByToken = func() map[string]Interval {
	m := make(map[string]Interval, len(intervals))
	for _, iv := range intervals {
		m[iv.Token] = iv
	}
	return m
}()

// AllowedIntervals returns the allowed Flux tokens, smallest first. Useful for
// error messages and OpenAPI enums (M10).
func AllowedIntervals() []string {
	out := make([]string, len(intervals))
	for i, iv := range intervals {
		out[i] = iv.Token
	}
	return out
}

// lookupInterval returns the Interval for a Flux token, or false if not allowed.
func lookupInterval(token string) (Interval, bool) {
	iv, ok := intervalByToken[token]
	return iv, ok
}

// DefaultInterval picks a sensible bucket size for a window when the caller
// supplies none:
//
//   - today → 1h (24-ish buckets)
//   - week  → 1d (7 buckets)
//   - month → 1d (~30 buckets)
//   - custom and rolling (<N>d/<N>h) → chosen by span so the bucket count stays
//     modest (≲ ~300): ≤2d → 1h, ≤14d → 6h, otherwise 1d.
func DefaultInterval(win Window) string {
	switch win.Label {
	case WindowToday:
		return "1h"
	case WindowWeek, WindowMonth:
		return "1d"
	}
	// custom + rolling windows: pick by elapsed span.
	span := win.Stop.Sub(win.Start)
	switch {
	case span <= 2*24*time.Hour:
		return "1h"
	case span <= 14*24*time.Hour:
		return "6h"
	default:
		return "1d"
	}
}

// ResolveInterval resolves the effective bucketing for a window. When requested
// is empty the smart default for the window is used; otherwise requested must
// be one of the allowed tokens. The resulting bucket count over the window is
// checked against MaxBuckets and rejected (with a message naming the cap and
// suggesting a coarser interval) if exceeded.
//
// loc is the timezone the axis is computed in (so calendar-day counts are
// DST-correct).
func ResolveInterval(win Window, requested string, loc *time.Location) (Interval, error) {
	if loc == nil {
		return Interval{}, fmt.Errorf("energy: nil location")
	}

	token := requested
	if token == "" {
		token = DefaultInterval(win)
	}

	iv, ok := lookupInterval(token)
	if !ok {
		return Interval{}, &IntervalNotAllowedError{
			Interval: requested,
			Allowed:  AllowedIntervals(),
		}
	}

	n := bucketCount(win, iv, loc)
	if n > MaxBuckets {
		return Interval{}, &BucketCapError{
			Interval:         token,
			Buckets:          n,
			MaxBuckets:       MaxBuckets,
			Suggested:        suggestCoarser(win, loc),
			MaxWindowSeconds: int64(MaxBuckets) * int64(iv.Duration/time.Second),
		}
	}

	return iv, nil
}

// suggestCoarser returns the smallest allowed interval whose bucket count over
// win is within MaxBuckets, defaulting to the coarsest ("1d") if even that is
// over (it never is for realistic windows).
func suggestCoarser(win Window, loc *time.Location) string {
	for _, iv := range intervals {
		if bucketCount(win, iv, loc) <= MaxBuckets {
			return iv.Token
		}
	}
	return intervals[len(intervals)-1].Token
}

// bucketCount returns how many buckets the canonical axis would have for win at
// iv. It shares the exact stepping logic of BucketStarts so the cap check and
// the axis can never disagree.
func bucketCount(win Window, iv Interval, loc *time.Location) int {
	return len(BucketStarts(win, iv, loc))
}

// BucketCapError is the refusal for a window that yields too many buckets.
//
// Typed rather than a bare fmt.Errorf so the handler can put the NUMBERS on the
// wire beside the prose (issue #36 N1). The message was already good — it says
// what is wrong, why the constraint exists and what to do — but it is unusable as
// data, and a caller chunking a price/consumption join has to reconcile a cap
// stated in BUCKETS here against one stated in DAYS on /prices. Nothing in the
// API told them how.
type BucketCapError struct {
	// Interval is the token the caller asked for.
	Interval string

	// Buckets is how many that interval yields over the window; MaxBuckets is the
	// cap it exceeded.
	Buckets    int
	MaxBuckets int

	// Suggested is the finest allowed interval that would fit.
	Suggested string

	// MaxWindowSeconds is the cap expressed as a WINDOW LENGTH at the requested
	// interval — the same unit /prices states its cap in, which is what lets one
	// auto-chunking routine serve both. It is 0 for a calendar interval whose real
	// length varies.
	MaxWindowSeconds int64
}

func (e *BucketCapError) Error() string {
	return fmt.Sprintf("energy: interval %q yields %d buckets over the window, exceeding the cap of %d; request a coarser interval (e.g. %q)",
		e.Interval, e.Buckets, e.MaxBuckets, e.Suggested)
}

// IntervalNotAllowedError is the refusal for an interval outside the allowed set.
//
// Typed for the same reason BucketCapError is, and because the two arrive from
// the SAME endpoint: a caller discovering constraints programmatically hits both,
// and getting structure from one and a sentence from the other means it still has
// to parse prose — which is the thing making caps machine-readable set out to
// stop.
//
// Reported in review as a live false positive: reaching for interval=1m to
// provoke a cap breach returns this instead, so a check for `limits` fails for a
// reason that has nothing to do with caps, and a retry loop concludes `limits` is
// unreliable rather than that it met a different rule.
type IntervalNotAllowedError struct {
	// Interval is the token the caller asked for.
	Interval string

	// Allowed is the whole accepted set, smallest first, so a client can pick
	// without knowing the vocabulary in advance.
	Allowed []string
}

func (e *IntervalNotAllowedError) Error() string {
	return fmt.Sprintf("energy: interval %q not allowed; choose one of %v", e.Interval, e.Allowed)
}
