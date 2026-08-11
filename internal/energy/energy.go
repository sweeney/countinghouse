// Package energy turns Influx telemetry into windowed kWh per device. It owns
// the routing decision (counter vs integral) and the row-reduction logic, but
// delegates all I/O to an influx.Querier so it is trivially testable.
package energy

import (
	"context"
	"fmt"
	"time"

	"github.com/sweeney/countinghouse/internal/influx"
)

// Path identifiers returned by PathForClass / DeviceWindowKWh.
const (
	PathCounter  = "counter"
	PathIntegral = "integral"
)

// counterClasses are the device classes that publish a monotonic energy_kwh
// field and therefore use the reset-safe increase() counter path. Routing is
// by class (== energy_kwh presence per PLAN §5); energy_strategy is ignored.
var counterClasses = map[string]bool{
	"continuous_power_device":  true,
	"cycle_power_device":       true,
	"short_burst_power_device": true,
	"media_power_device":       true,
	EnergyMeterClass:           true,
}

// PathForClass maps a device class to its query path. It returns ("counter",
// true) for plug classes and the energy meter, ("integral", true) for
// ups_sensor, and ("", false) for any unknown class.
func PathForClass(class string) (string, bool) {
	if counterClasses[class] {
		return PathCounter, true
	}
	if class == "ups_sensor" {
		return PathIntegral, true
	}
	return "", false
}

// DeviceWindowKWh computes energy consumed by a single device over
// [start, stop). It selects the query path from class, builds the matching
// Flux, runs it via q, and reduces the rows to one kWh value.
//
// Reduction: both query paths reduce to one row PER TABLE, not one row overall
// (counter: last() of the increase; integral: the W·h→kWh map). Influx starts a
// new table whenever a tag value changes inside the window — a room rename, a
// class correction, a new site tag — so a device's energy arrives split across
// several rows, each a distinct slice of the same window. We therefore SUM the
// rows: taking only the last silently discarded every earlier slice, which made
// a longer window report less energy than a window contained within it.
//
// The sum omits the delta across a table boundary itself (the gap between one
// tag set's last sample and the next's first), which is bounded by one sample
// interval and cannot be recovered without a single-table query.
//
// An empty result (device offline / no data in window) is 0 kWh with no error.
// source is the path used ("counter"/"integral"). An unknown class is an error.
func DeviceWindowKWh(ctx context.Context, q influx.Querier, bucket, deviceID, class string, start, stop time.Time) (kwh float64, source string, err error) {
	path, ok := PathForClass(class)
	if !ok {
		return 0, "", fmt.Errorf("energy: unknown device class %q", class)
	}

	var flux string
	switch path {
	case PathCounter:
		flux = influx.BuildCounterFlux(bucket, deviceID, start, stop)
	case PathIntegral:
		flux = influx.BuildIntegralFlux(bucket, deviceID, start, stop)
	}

	rows, err := q.Query(ctx, flux)
	if err != nil {
		return 0, path, err
	}
	// Every row is one table's total for the window; the device's energy is
	// their sum. An empty result sums to 0, which is the wanted answer anyway.
	var total float64
	for _, r := range rows {
		total += r.Value
	}
	return total, path, nil
}
