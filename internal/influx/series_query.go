package influx

import (
	"fmt"
	"strings"
	"time"
)

// deviceSet renders a Flux array literal of quoted device ids, e.g.
// `["a", "b"]`. It is used inside contains(value: r.device_id, set: [...]) so a
// single query can fan out across a whole set of devices (keeping the query
// count device-count-independent).
func deviceSet(deviceIDs []string) string {
	quoted := make([]string, 0, len(deviceIDs))
	for _, id := range deviceIDs {
		// Fail closed: drop any id that is not a safe identifier so it can
		// never be interpolated into the Flux array literal (see validID).
		if !validID(id) {
			continue
		}
		quoted = append(quoted, fmt.Sprintf("%q", id))
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

// padStart returns the query range start moved one interval earlier than the
// window start. The counter series needs a real datapoint immediately before
// the first window bucket so that difference() yields a proper delta for bucket
// 0; the Go layer then drops every bucket before the window. The pad amount is
// the literal local-window duration of one interval (sub-day intervals are
// fixed; for "1d" we subtract 24h, which is close enough to seed the pad — the
// Go axis, not this start, is authoritative for bucket boundaries).
func padStart(start time.Time, interval string) time.Time {
	return start.Add(-padDuration(interval))
}

// intervalDurations maps each allowed Flux duration token to its Go duration.
// It mirrors energy.intervals, which owns the allowed set; this package cannot
// import that one (energy imports influx), so the table is kept minimal and
// every consumer here treats an unknown token explicitly.
var intervalDurations = map[string]time.Duration{
	"5m":  5 * time.Minute,
	"15m": 15 * time.Minute,
	"30m": 30 * time.Minute,
	"1h":  time.Hour,
	"6h":  6 * time.Hour,
	"1d":  24 * time.Hour,
}

// intervalDuration resolves a Flux duration token, reporting false for one
// outside the allowed set. Note "1d" is its NOMINAL 24h: a calendar day across a
// DST change is 23h or 25h, and callers that care step by calendar date instead.
func intervalDuration(token string) (time.Duration, bool) {
	d, ok := intervalDurations[token]
	return d, ok
}

// padDuration maps a Flux duration token to a Go duration for the pad. It is
// deliberately lenient: any unrecognised token falls back to one hour, which is
// safe because the pad only needs to guarantee at least one prior datapoint.
func padDuration(interval string) time.Duration {
	if d, ok := intervalDuration(interval); ok {
		return d
	}
	return time.Hour
}

// BuildCounterSeriesFlux builds the per-bucket energy series from the cumulative
// energy_kwh counter, for a SET of counter-class devices (plug classes + the
// energy meter). It is reset-safe and timezone-aware:
//
//   - increase() runs FIRST, BEFORE aggregateWindow, so device-side counter
//     resets are absorbed into the monotonic running total.
//   - aggregateWindow(every: interval, fn: last, location: timezone.location(tz),
//     createEmpty: true) collapses each bucket to its closing running total on
//     DST-aware local boundaries, emitting empty buckets so the axis is dense.
//   - difference() turns the per-bucket running totals into per-bucket deltas
//     (the energy consumed within each bucket).
//
// The query range is padded ONE interval before start (see padStart) so the
// first real window bucket has a prior value to difference against; the Go
// layer drops the pad bucket(s). Rows keep r.device_id (group columns are
// preserved) so the caller can demux per device.
func BuildCounterSeriesFlux(bucket string, deviceIDs []string, start, stop time.Time, interval, tz string) string {
	return fmt.Sprintf(`import "timezone"

from(bucket: %q)
  |> range(start: %s, stop: %s)
  |> filter(fn: (r) => r._measurement == "device_power" and r._field == "energy_kwh")
  |> filter(fn: (r) => contains(value: r.device_id, set: %s))
%s
  |> increase()
  |> aggregateWindow(every: %s, fn: last, timeSrc: "_start", location: timezone.location(name: %q), createEmpty: true)
  |> difference()`,
		bucket,
		fluxTime(padStart(start, interval)),
		fluxTime(stop),
		deviceSet(deviceIDs),
		regroupByDevice,
		interval,
		tz,
	)
}

// BuildCounterHeadFlux builds the EXACT windowed energy delta over an arbitrary
// range for a SET of counter devices — the same increase()|>last() reduction
// BuildCounterFlux uses for the whole-window endpoints, fanned out across a
// device set so the query count stays device-count-independent.
//
// It exists to repair the FIRST bucket of the counter series (issue #27).
// BuildCounterSeriesFlux buckets on aggregateWindow's grid, which is anchored at
// local midnight and therefore begins at or BEFORE the window start; bucket 0's
// difference() delta is consequently the whole grid interval, and the window
// start never enters the arithmetic. A grid interval is not divisible after the
// fact, so the in-window head gets its own query over [win.Start, buckets[1]).
// increase() is already correct over an arbitrary range — which is exactly why
// the scalar endpoints get this right today — so the two agree by construction.
//
// The caller issues this ONLY when win.Start is strictly after the first bucket
// start; every midnight-aligned window (today/week/month/<N>d) skips it.
func BuildCounterHeadFlux(bucket string, deviceIDs []string, start, stop time.Time) string {
	return fmt.Sprintf(`from(bucket: %q)
  |> range(start: %s, stop: %s)
  |> filter(fn: (r) => r._measurement == "device_power" and r._field == "energy_kwh")
  |> filter(fn: (r) => contains(value: r.device_id, set: %s))
%s
  |> increase()
  |> last()`,
		bucket,
		fluxTime(start),
		fluxTime(stop),
		deviceSet(deviceIDs),
		regroupByDeviceWindow,
	)
}

// BuildPowerMeanSeriesFlux builds the per-bucket mean instantaneous power
// series (power_w) for a SET of devices, on DST-aware local buckets. It is used
// for two purposes by the energy layer:
//
//   - average power: the bucket mean is the avg_w reported directly.
//   - UPS energy: mean watts × bucket-hours / 1000 → kWh (computed in Go, since
//     bucket-hours vary across a DST changeover).
//
// Unlike the counter series this needs no pad: a bucket mean is self-contained.
// createEmpty: true keeps the axis dense; rows keep r.device_id for demuxing.
func BuildPowerMeanSeriesFlux(bucket string, deviceIDs []string, start, stop time.Time, interval, tz string) string {
	return fmt.Sprintf(`import "timezone"

from(bucket: %q)
  |> range(start: %s, stop: %s)
  |> filter(fn: (r) => r._measurement == "device_power" and r._field == "power_w")
  |> filter(fn: (r) => contains(value: r.device_id, set: %s))
%s
  |> aggregateWindow(every: %s, fn: mean, timeSrc: "_start", location: timezone.location(name: %q), createEmpty: true)`,
		bucket,
		fluxTime(start),
		fluxTime(stop),
		deviceSet(deviceIDs),
		regroupByDevice,
		interval,
		tz,
	)
}
