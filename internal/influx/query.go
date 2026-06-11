// Package influx is countinghouse's read-side query layer over InfluxDB.
//
// Countinghouse never writes to Influx; it only queries the per-device
// telemetry statehouse wrote, turning it into windowed kWh. The Querier
// interface is the testable seam (mirroring statehouse's local-interface +
// fake-double pattern on the write side): production uses Client, tests use
// FakeQuerier, and neither the energy logic nor the handlers ever touch the
// influxdb2 SDK directly.
package influx

import (
	"context"
	"fmt"
	"time"
)

// Row is a single decoded Flux record. It flattens the tags and columns we
// care about out of a query.FluxRecord. Not every query yields every field:
// callers should treat absent tags as empty strings (the Client decodes them
// defensively).
type Row struct {
	DeviceID string
	Class    string
	Location string
	Field    string
	// Value holds the record's _value when it is numeric (float64); it is the
	// zero value for string-valued records.
	Value float64
	// Text holds the record's _value when it is a string. The device_activity
	// measurement publishes string `from`/`to` fields (e.g. "idle"→"active"),
	// which the numeric Value cannot carry; Text preserves them. Empty for
	// numeric records.
	Text string
	Time time.Time
}

// Querier is the minimal subset of Influx querying that countinghouse needs.
// Defining it locally keeps the energy logic decoupled from the SDK and lets
// tests substitute FakeQuerier without standing up a database.
type Querier interface {
	// Query runs a Flux script and returns the decoded rows. An error is
	// returned for transport/auth failures or a malformed result; an empty
	// (but non-nil-error) result simply yields a nil/empty slice.
	Query(ctx context.Context, flux string) ([]Row, error)
	// Ping reports whether the backend is reachable.
	Ping(ctx context.Context) bool
}

// fluxTime renders t for use inside range(start:, stop:). Influx accepts
// RFC3339; we force UTC so the bounds are unambiguous regardless of the
// caller's location.
func fluxTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

// BuildCounterFlux builds the reset-safe counter query for plug-class devices
// and the energy meter, which publish a monotonic energy_kwh field. increase()
// sums positive deltas (tolerating device-side counter resets) and last()
// collapses the running total to a single point for the window.
//
// energy_kwh is written as its own single-field point (it does not share rows
// with power_w/voltage_v), so we filter strictly on _field == "energy_kwh".
func BuildCounterFlux(bucket, deviceID string, start, stop time.Time) string {
	return fmt.Sprintf(`from(bucket: %q)
  |> range(start: %s, stop: %s)
  |> filter(fn: (r) => r._measurement == "device_power" and r._field == "energy_kwh")
  |> filter(fn: (r) => r.device_id == %q)
  |> increase()
  |> last()`, bucket, fluxTime(start), fluxTime(stop), deviceID)
}

// BuildActivityFlux builds the binary/event-timeline query over the
// device_activity measurement for a SET of devices. Each state transition is
// written as two single-field string points (`from` and `to`) sharing one
// timestamp — e.g. an "idle"→"active" edge writes from="idle" and to="active"
// at the transition time. This builder returns the RAW field rows (no pivot):
// the events package pairs the from/to rows by _time. Keeping the pivot out of
// Flux keeps the influx layer simple and the string handling explicit (Rows
// carry the string label in Text).
//
// device_id and class tags plus _field/_value/_time are preserved so the events
// package can demux per device and normalise the on/off state.
func BuildActivityFlux(bucket string, deviceIDs []string, start, stop time.Time) string {
	return fmt.Sprintf(`from(bucket: %q)
  |> range(start: %s, stop: %s)
  |> filter(fn: (r) => r._measurement == "device_activity")
  |> filter(fn: (r) => contains(value: r.device_id, set: %s))`,
		bucket, fluxTime(start), fluxTime(stop), deviceSet(deviceIDs))
}

// BuildActivityCarryInFlux builds the carry-in query: the single most-recent
// device_activity transition BEFORE the window start, per device. Its `to`
// field is the state each device was in as the window opened, which lets the
// events package open the first interval correctly even when no transition
// happens inside the window.
//
// The range starts at the Unix epoch (time 0) so the query reaches arbitrarily
// far back; group(columns: ["device_id"]) + last() collapses to one row per
// device. As with BuildActivityFlux the from/to rows are returned raw and the
// string label rides in Row.Text.
func BuildActivityCarryInFlux(bucket string, deviceIDs []string, start time.Time) string {
	return fmt.Sprintf(`from(bucket: %q)
  |> range(start: 1970-01-01T00:00:00Z, stop: %s)
  |> filter(fn: (r) => r._measurement == "device_activity")
  |> filter(fn: (r) => contains(value: r.device_id, set: %s))
  |> group(columns: ["device_id", "_field"])
  |> last()`,
		bucket, fluxTime(start), deviceSet(deviceIDs))
}

// BuildIntegralFlux builds the time-integral query for UPS sensors, which only
// publish instantaneous power_w. integral(unit: 1h) yields watt-hours over the
// window (linear interpolation bridges offline gaps); the final map converts
// W·h to kWh by dividing by 1000.
//
// power_w is its own single-field point, so we filter strictly on
// _field == "power_w".
func BuildIntegralFlux(bucket, deviceID string, start, stop time.Time) string {
	return fmt.Sprintf(`from(bucket: %q)
  |> range(start: %s, stop: %s)
  |> filter(fn: (r) => r._measurement == "device_power" and r._field == "power_w")
  |> filter(fn: (r) => r.device_id == %q)
  |> integral(unit: 1h, interpolate: "linear")
  |> map(fn: (r) => ({ r with _value: r._value / 1000.0 }))`,
		bucket, fluxTime(start), fluxTime(stop), deviceID)
}
