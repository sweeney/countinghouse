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
// step the canonical axis and to convert UPS mean-power to energy. Calendar
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
//   - custom → chosen by span so the bucket count stays modest (≲ ~300):
//     ≤2d → 1h, ≤14d → 6h, otherwise 1d.
func DefaultInterval(win Window) string {
	switch win.Label {
	case WindowToday:
		return "1h"
	case WindowWeek, WindowMonth:
		return "1d"
	case WindowCustom:
		span := win.Stop.Sub(win.Start)
		switch {
		case span <= 2*24*time.Hour:
			return "1h"
		case span <= 14*24*time.Hour:
			return "6h"
		default:
			return "1d"
		}
	default:
		return "1h"
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
		return Interval{}, fmt.Errorf("energy: interval %q not allowed; choose one of %v", requested, AllowedIntervals())
	}

	n := bucketCount(win, iv, loc)
	if n > MaxBuckets {
		coarser := suggestCoarser(win, loc)
		return Interval{}, fmt.Errorf("energy: interval %q yields %d buckets over the window, exceeding the cap of %d; request a coarser interval (e.g. %q)", token, n, MaxBuckets, coarser)
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
