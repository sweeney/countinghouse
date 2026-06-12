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
	quoted := make([]string, len(deviceIDs))
	for i, id := range deviceIDs {
		quoted[i] = fmt.Sprintf("%q", id)
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

// padDuration maps a Flux duration token to a Go duration for the pad. It is
// deliberately lenient: any unrecognised token falls back to one hour, which is
// safe because the pad only needs to guarantee at least one prior datapoint.
func padDuration(interval string) time.Duration {
	switch interval {
	case "5m":
		return 5 * time.Minute
	case "15m":
		return 15 * time.Minute
	case "30m":
		return 30 * time.Minute
	case "1h":
		return time.Hour
	case "6h":
		return 6 * time.Hour
	case "1d":
		return 24 * time.Hour
	default:
		return time.Hour
	}
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
  |> increase()
  |> aggregateWindow(every: %s, fn: last, timeSrc: "_start", location: timezone.location(name: %q), createEmpty: true)
  |> difference()`,
		bucket,
		fluxTime(padStart(start, interval)),
		fluxTime(stop),
		deviceSet(deviceIDs),
		interval,
		tz,
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
  |> aggregateWindow(every: %s, fn: mean, timeSrc: "_start", location: timezone.location(name: %q), createEmpty: true)`,
		bucket,
		fluxTime(start),
		fluxTime(stop),
		deviceSet(deviceIDs),
		interval,
		tz,
	)
}
