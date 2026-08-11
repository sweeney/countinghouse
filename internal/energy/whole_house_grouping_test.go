package energy

import (
	"testing"
	"time"

	"github.com/sweeney/countinghouse/internal/config"
)

// A device whose readings describe the whole property has to land somewhere in a room
// grouping. Dropping it breaks the invariant documented at withUnmonitoredCatchAll —
// "the grouped series partition exactly the monitored devices" — because houseParts
// counts every metered device in `monitored` regardless of its place. The energy then
// inflates `monitored`, appears in no room series, and `include_unmonitored` no longer
// sums to the meter.
//
// Attributing it to the room it sits in would be the conflation this migration
// removes, relocated from `location` to `room`. So it gets its own key.

func wholeHouseBuckets() []time.Time {
	return []time.Time{time.Date(2026, 8, 11, 0, 0, 0, 0, time.UTC)}
}

func wholeHouseSetup(immersion config.DeviceConfig) ([]Series, map[string][]float64) {
	devices := map[string]config.DeviceConfig{
		"immersion":         immersion,
		"fridge":            {Class: "continuous_power_device", Location: "kitchen"},
		"electricity_meter": {Class: EnergyMeterClass, Location: "meter"},
	}
	energy := map[string][]float64{"immersion": {2}, "fridge": {1}, "electricity_meter": {5}}
	power := map[string][]float64{}

	return AssembleSeries(wholeHouseBuckets(), nil, devices, energy, power, testTariff(), GroupByRoom), energy
}

// TestWholeHouseDeviceGetsItsOwnKey pins the decision: legacy `location: house` and a
// migrated `room` + `covers: house` must group identically, so republishing the
// namespace cannot silently move energy between series.
func TestWholeHouseDeviceGetsItsOwnKey(t *testing.T) {
	for name, immersion := range map[string]config.DeviceConfig{
		"legacy location": {Class: "cycle_power_device", Location: "house"},
		"migrated covers": {Class: "cycle_power_device", Room: "groundfloor.boiler-room", Covers: "house"},
	} {
		t.Run(name, func(t *testing.T) {
			out, _ := wholeHouseSetup(immersion)

			keys := map[string]float64{}
			for _, s := range out {
				keys[s.Key] = s.TotalKWh
			}
			if got, ok := keys["house"]; !ok || got != 2 {
				t.Errorf("house series = %v (present=%v), want 2 kWh; keys were %v", got, ok, keys)
			}
			if keys["groundfloor.boiler-room"] != 0 {
				t.Errorf("whole-property energy was attributed to the room it sits in: %v", keys)
			}
			if keys["kitchen"] != 1 {
				t.Errorf("kitchen = %v, want 1", keys["kitchen"])
			}
		})
	}
}

// The invariant the drop was breaking: every metered device counted in `monitored`
// must appear in exactly one grouped series, so grouping + unmonitored sums to the meter.
func TestGroupedSeriesPartitionTheMonitoredDevices(t *testing.T) {
	out, _ := wholeHouseSetup(config.DeviceConfig{Class: "cycle_power_device", Location: "house"})

	var grouped float64
	for _, s := range out {
		grouped += s.TotalKWh
	}
	// immersion 2 + fridge 1; the meter itself is excluded from the grouping.
	if grouped != 3 {
		t.Errorf("grouped series total = %v, want 3 — the parts no longer partition the monitored devices", grouped)
	}
}
