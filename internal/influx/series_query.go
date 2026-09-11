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

// BuildCounterSeriesFlux builds the per-bucket CLOSING RUNNING TOTAL of the
// cumulative energy_kwh counter, for a SET of counter-class devices (plug
// classes + the energy meter). It is reset-safe and timezone-aware:
//
//   - The range is the EXACT window. increase() therefore re-bases at the first
//     reading at or after `start`, so every value it produces is "energy since
//     the window opened" — the same zero point BuildCounterFlux uses for
//     /devices/{id}/energy. It also runs FIRST, before aggregateWindow, so
//     device-side counter resets are absorbed into the monotonic running total.
//   - aggregateWindow(every:, fn: last, timeSrc: "_start", location:) collapses
//     each bucket to its closing running total on DST-aware local boundaries.
//
// It deliberately does NOT difference() — the caller does that in Go, walking
// the canonical axis and carrying the last known total across buckets with no
// readings (see energy.demuxCounterTotals). Two reasons, both learned the hard
// way (issue #29):
//
//   - difference() consumes the first window it sees as its seed. The old design
//     paid for that seed by padding the range one interval earlier, which
//     anchored the whole series at whatever reading the device last managed
//     BEFORE the window — energy from outside the window, billed. And when the
//     pad held no reading, the seed came from INSIDE the window instead and a
//     real bucket's energy vanished.
//   - Differencing in Go makes a reading gap explicit: the buckets it spans are
//     zero and the next real reading resumes from the carried total, so nothing
//     is lost and nothing pre-`from` leaks in.
//
// createEmpty is FALSE for the same reason: an empty bucket would arrive as a
// null that decodes to 0.0, indistinguishable from a running total of zero (a
// counter reset). Omitting those buckets and carrying forward in Go is
// unambiguous.
//
// Rows keep r.device_id (group columns are preserved) so the caller can demux
// per device.
func BuildCounterSeriesFlux(bucket string, deviceIDs []string, start, stop time.Time, interval, tz string) string {
	return fmt.Sprintf(`import "timezone"

from(bucket: %q)
  |> range(start: %s, stop: %s)
  |> filter(fn: (r) => r._measurement == "device_power" and r._field == "energy_kwh")
  |> filter(fn: (r) => contains(value: r.device_id, set: %s))
%s
  |> increase()
  |> aggregateWindow(every: %s, fn: last, timeSrc: "_start", location: timezone.location(name: %q), createEmpty: false)`,
		bucket,
		fluxTime(start),
		fluxTime(stop),
		deviceSet(deviceIDs),
		regroupByDevice,
		interval,
		tz,
	)
}

// BuildPowerIntegralSeriesFlux builds the per-bucket ENERGY series for
// power-only devices (ups_sensor), by integrating power_w over each bucket —
// the same reduction BuildIntegralFlux applies to the whole window, just
// windowed, so /series and /devices/{id}/energy estimate the UPS integral the
// same way (issue #32).
//
// It replaces mean(power_w) x bucket_hours. That was a different estimator of
// the same quantity: a bucket mean weights every sample equally no matter how
// long it stood, while a trapezoidal integral weights by elapsed time. The two
// agree for evenly-spaced samples — which is why this was invisible for two
// steadily-reporting UPSs — and diverge exactly when reporting turns uneven,
// i.e. when you most want the number.
//
// Two details are load-bearing:
//
//   - fn is a LAMBDA. aggregateWindow calls fn(column:, tables:<-), and
//     integral takes `columns` (plural) plus unit/interpolate, so it cannot be
//     passed by name the way mean or last can.
//   - the regroup is regroupByDevice, NOT the window variant BuildIntegralFlux
//     uses. integral reads its bounds from _start/_stop in the group key, and
//     aggregateWindow's internal window() supplies those per bucket; grouping on
//     them UPSTREAM would instead make every bucket its own table (see
//     regroupByDevice). The two integral builders therefore regroup differently
//     ON PURPOSE, which is the one place they are allowed to differ.
//
// createEmpty is false: a bucket the device did not report in has nothing to
// integrate, and an empty bucket would arrive as a null decoding to 0.0 —
// "drew nothing" wearing the clothes of "said nothing" (see Row.Null). Omitting
// it keeps that distinction alive for the caller and for C13 staleness.
//
// The trailing map converts W.h to kWh, exactly as BuildIntegralFlux does.
func BuildPowerIntegralSeriesFlux(bucket string, deviceIDs []string, start, stop time.Time, interval, tz string) string {
	return fmt.Sprintf(`import "timezone"

from(bucket: %q)
  |> range(start: %s, stop: %s)
  |> filter(fn: (r) => r._measurement == "device_power" and r._field == "power_w")
  |> filter(fn: (r) => contains(value: r.device_id, set: %s))
%s
  |> aggregateWindow(every: %s, fn: (column, tables=<-) => tables |> integral(unit: 1h, interpolate: "linear"), timeSrc: "_start", location: timezone.location(name: %q), createEmpty: false)
  |> map(fn: (r) => ({ r with _value: r._value / 1000.0 }))`,
		bucket,
		fluxTime(start),
		fluxTime(stop),
		deviceSet(deviceIDs),
		regroupByDevice,
		interval,
		tz,
	)
}

// BuildPowerMeanSeriesFlux builds the per-bucket mean instantaneous power
// series (power_w) for a SET of devices, on DST-aware local buckets. It serves
// avg_w for counter-class devices: the bucket mean IS the average power, so it
// is reported directly.
//
// It no longer carries UPS energy. That used mean watts × bucket-hours, a
// different estimator from the integral the scalar endpoint applies — see
// BuildPowerIntegralSeriesFlux, which took the job (issue #32).
//
// A bucket mean is self-contained, so this reduces to one value per bucket with
// no cross-bucket arithmetic and nothing to seed. createEmpty: true keeps the
// axis dense; rows keep r.device_id for demuxing.
//
// The density is still bought with nulls for unreported buckets, but they are no
// longer silently worth 0 W: Client.Query flags them (Row.Null) and energy.demux
// drops them, so an absent bucket stays absent and a device that produced only
// nulls has no powerByDevice entry at all — which is precisely what C13
// staleness looks for.
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
