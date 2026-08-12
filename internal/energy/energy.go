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
// Reduction: the Flux builders now regroup a device's tag-fragments into ONE table
// before reducing (see influx.regroupByDeviceWindow), so both paths return exactly one
// row and this reduction is a formality. It is written to stay correct if that invariant
// ever weakens, and the two paths need OPPOSITE handling for that case:
//
//   - counter: increase() accumulates only over the points in its own table, so
//     fragments hold disjoint slices of the window and their totals ADD.
//   - integral: integral() reads its bounds from the group key's _start/_stop — the
//     whole window — so each fragment is extrapolated across all of it. Fragments are
//     competing ESTIMATES OF THE SAME QUANTITY, not addends. Summing them is what
//     tripled the UPS bill (5.533 kWh against a true 1.837); one estimate is roughly
//     right, so take one rather than multiply the error.
//
// Getting this backwards is expensive and silent, which is why both directions are
// pinned by tests rather than left to the comment.
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
	if len(rows) == 0 {
		return 0, path, nil
	}
	if path == PathIntegral {
		// Competing estimates of the same window, never addends — see above.
		return rows[len(rows)-1].Value, path, nil
	}
	// Counter: disjoint accumulations, so they add.
	var total float64
	for _, r := range rows {
		total += r.Value
	}
	return total, path, nil
}
