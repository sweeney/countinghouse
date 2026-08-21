package energy

import (
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/sweeney/countinghouse/internal/config"
)

// The whole-house meter is excluded from the FLEET groupings (device/room/class)
// because it is the authoritative total, not one contributor among the plugs —
// including it there would double-count. But GET /devices/{id}/series builds over
// a single-device inventory, and when that device IS the meter the same exclusion
// emptied the response: 200 with "series": null, which consumers read as "this
// device has no readings" (issue #21).
//
// A single-device request is not a fleet and cannot double-count, so it uses
// GroupBySelf: device shape, no whole-house exclusion.

func meterOnlyInventory() map[string]config.DeviceConfig {
	return map[string]config.DeviceConfig{
		"electricity_meter": {Class: EnergyMeterClass, Room: "basement.hallway", Covers: "house", DisplayName: "Electricity Meter"},
	}
}

func meterFullInventory() map[string]config.DeviceConfig {
	return map[string]config.DeviceConfig{
		"electricity_meter": {Class: EnergyMeterClass, Room: "basement.hallway", Covers: "house", DisplayName: "Electricity Meter"},
		"fridge":            {Class: "continuous_power_device", Room: "groundfloor.kitchen", DisplayName: "Fridge"},
	}
}

func meterBuckets() []time.Time {
	loc := time.UTC
	return []time.Time{
		time.Date(2026, 8, 21, 0, 0, 0, 0, loc),
		time.Date(2026, 8, 21, 1, 0, 0, 0, loc),
	}
}

// The reported bug, at the layer that caused it: the assembler dropped the meter
// from a single-device inventory, leaving no series at all.
func TestSelfGroupingIncludesTheMeter(t *testing.T) {
	energyBy := map[string][]float64{"electricity_meter": {0.5, 0.75}}
	powerBy := map[string][]float64{"electricity_meter": {500, 750}}

	out := AssembleSeries(meterBuckets(), []float64{1, 1}, meterOnlyInventory(), energyBy, powerBy, testTariff(), GroupBySelf)

	if len(out) != 1 {
		t.Fatalf("want 1 series for the meter, got %d: %+v", len(out), out)
	}
	s := out[0]
	if s.Key != "electricity_meter" {
		t.Errorf("key = %q, want the device id", s.Key)
	}
	if s.Label != "Electricity Meter" {
		t.Errorf("label = %q, want the display name", s.Label)
	}
	if s.Class != EnergyMeterClass {
		t.Errorf("class = %q, want %q", s.Class, EnergyMeterClass)
	}
	if s.Room != "basement.hallway" {
		t.Errorf("room = %q, want basement.hallway", s.Room)
	}
	if !reflect.DeepEqual(s.KWh, []float64{0.5, 0.75}) {
		t.Errorf("kwh = %v, want [0.5 0.75]", s.KWh)
	}
	if s.TotalKWh != 1.25 {
		t.Errorf("total_kwh = %v, want 1.25", s.TotalKWh)
	}
}

// The self grouping must agree bucket-for-bucket with the house grouping's
// "meter" series — they are the same quantity reached by two routes, and the
// issue was reported precisely because those routes disagreed.
func TestSelfGroupingMatchesHouseMeterSeries(t *testing.T) {
	energyBy := map[string][]float64{"electricity_meter": {0.5, 0.75}, "fridge": {0.1, 0.2}}
	powerBy := map[string][]float64{"electricity_meter": {500, 750}, "fridge": {100, 200}}
	buckets, hrs := meterBuckets(), []float64{1, 1}

	self := AssembleSeries(buckets, hrs, meterOnlyInventory(), energyBy, powerBy, testTariff(), GroupBySelf)
	house := AssembleSeries(buckets, hrs, meterFullInventory(), energyBy, powerBy, testTariff(), GroupByHouse)

	meter := OnlySeries(house, "meter")
	if len(self) != 1 || len(meter) != 1 {
		t.Fatalf("want one series each, got self=%d house-meter=%d", len(self), len(meter))
	}
	if !reflect.DeepEqual(self[0].KWh, meter[0].KWh) {
		t.Errorf("kwh: self %v != house meter %v", self[0].KWh, meter[0].KWh)
	}
	if !reflect.DeepEqual(self[0].Cost, meter[0].Cost) {
		t.Errorf("cost: self %v != house meter %v", self[0].Cost, meter[0].Cost)
	}
	if !reflect.DeepEqual(self[0].AvgW, meter[0].AvgW) {
		t.Errorf("avg_w: self %v != house meter %v", self[0].AvgW, meter[0].AvgW)
	}
	if self[0].TotalKWh != meter[0].TotalKWh || self[0].TotalCost != meter[0].TotalCost {
		t.Errorf("totals differ: self %+v vs house meter %+v", self[0], meter[0])
	}
}

// The fix must not leak the meter into the fleet groupings, which is what the
// exclusion is for: the meter would double-count against the plugs it measures.
func TestFleetGroupingsStillExcludeTheMeter(t *testing.T) {
	// Two plugs, in different rooms and of different classes, so each grouping
	// has more than one member and the total is genuinely accumulated: a
	// single-member total would compare exactly by luck and hide the tolerance
	// this assertion needs.
	inv := meterFullInventory()
	inv["kettle"] = config.DeviceConfig{Class: "short_burst_power_device", Room: "groundfloor.utility", DisplayName: "Kettle"}
	energyBy := map[string][]float64{"electricity_meter": {0.5, 0.75}, "fridge": {0.05, 0.05}, "kettle": {0.1, 0.1}}
	powerBy := map[string][]float64{"electricity_meter": {500, 750}, "fridge": {50, 50}, "kettle": {100, 100}}

	for _, groupBy := range []string{GroupByDevice, GroupByRoom, GroupByClass} {
		out := AssembleSeries(meterBuckets(), []float64{1, 1}, inv, energyBy, powerBy, testTariff(), groupBy)
		for _, s := range out {
			if s.Key == "electricity_meter" || s.Class == EnergyMeterClass {
				t.Errorf("group_by=%s leaked the meter: %+v", groupBy, s)
			}
		}
		// Compared with a tolerance (the package convention, cost_test.go): a
		// leak is what this assertion is for, and float representation error in
		// an accumulated total would report itself as one.
		var total float64
		for _, s := range out {
			total += s.TotalKWh
		}
		if math.Abs(total-0.3) > eps {
			t.Errorf("group_by=%s total = %v, want 0.3 (the two plugs, meter excluded)", groupBy, total)
		}
	}
}

// A plug device must be unaffected by the self grouping: same series it always got.
func TestSelfGroupingMatchesDeviceGroupingForAPlug(t *testing.T) {
	inv := map[string]config.DeviceConfig{
		"fridge": {Class: "continuous_power_device", Room: "groundfloor.kitchen", DisplayName: "Fridge"},
	}
	energyBy := map[string][]float64{"fridge": {0.1, 0.2}}
	powerBy := map[string][]float64{"fridge": {100, 200}}

	self := AssembleSeries(meterBuckets(), []float64{1, 1}, inv, energyBy, powerBy, testTariff(), GroupBySelf)
	dev := AssembleSeries(meterBuckets(), []float64{1, 1}, inv, energyBy, powerBy, testTariff(), GroupByDevice)
	if !reflect.DeepEqual(self, dev) {
		t.Errorf("self grouping changed a plug's series:\n self = %+v\n dev  = %+v", self, dev)
	}
}

// A non-metered class has no energy series under ANY grouping: the handler gate
// (PathForClass) rejects it before assembly, and assembly must agree.
func TestSelfGroupingSkipsNonMeteredClasses(t *testing.T) {
	inv := map[string]config.DeviceConfig{"hall-sensor": {Class: "environmental_sensor", Room: "groundfloor.hall"}}
	out := AssembleSeries(meterBuckets(), []float64{1, 1}, inv, nil, nil, testTariff(), GroupBySelf)
	if len(out) != 0 {
		t.Errorf("want no series for a non-metered class, got %+v", out)
	}
}

// One predicate decides "is this the authoritative whole-house total?", so the
// device MeterID names is exactly the device the fleet groupings leave out and
// the house grouping surfaces as "meter". When those answers came from separate
// inline class comparisons they could drift; issue #21 is what drift looks like.
func TestMeterIDAgreesWithTheFleetExclusion(t *testing.T) {
	inv := meterFullInventory()
	inv["hall-sensor"] = config.DeviceConfig{Class: "environmental_sensor", Room: "groundfloor.hall"}
	inv["immersion"] = config.DeviceConfig{Class: "cycle_power_device", Room: "basement.boiler-room", Covers: "house"}

	meterID, ok := MeterID(inv)
	if !ok {
		t.Fatal("MeterID found no meter")
	}

	for id, d := range inv {
		if got, want := IsWholeHouseTotal(d), id == meterID; got != want {
			t.Errorf("IsWholeHouseTotal(%s) = %v, want %v (MeterID = %s)", id, got, want, meterID)
		}
	}

	// A whole-property device that is NOT the meter stays in the monitored set:
	// `covers` says which place the readings describe, not that they are the
	// authoritative total. Excluding it would drop real consumption out of every
	// grouping and inflate "unmonitored" by it.
	if IsWholeHouseTotal(inv["immersion"]) {
		t.Error("a covers:house device that is not the meter was treated as the whole-house total")
	}
}
