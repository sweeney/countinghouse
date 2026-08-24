package energy

import (
	"testing"
	"time"

	"github.com/sweeney/countinghouse/internal/config"
)

// floorInventory: two devices on floor1 (in different rooms), one on floor2, the
// whole-house meter, a whole-property device, and one device the namespace
// declares no floor for.
func floorInventory() map[string]config.DeviceConfig {
	return map[string]config.DeviceConfig{
		"winefridge":        {Class: "continuous_power_device", Room: "floor1.room-a", Floor: "floor1"},
		"toaster":           {Class: "short_burst_power_device", Room: "floor1.room-c", Floor: "floor1"},
		"office_pc":         {Class: "media_power_device", Room: "floor2.room-b", Floor: "floor2"},
		"electricity_meter": {Class: EnergyMeterClass, Room: "floor1.room-c", Floor: "floor1", Covers: "house"},
		"immersion":         {Class: "continuous_power_device", Room: "floor1.room-c", Floor: "floor1", Covers: "house"},
		"unplaced":          {Class: "media_power_device", Room: "floor2.room-b"},
	}
}

func floorEnergy() map[string][]float64 {
	return map[string][]float64{
		"winefridge":        {0.05, 0.05},
		"toaster":           {0.10, 0.20},
		"office_pc":         {0.30, 0.30},
		"electricity_meter": {5, 5},
		"immersion":         {1, 1},
		"unplaced":          {0.40, 0.40},
	}
}

func twoBuckets(t *testing.T) []time.Time {
	t.Helper()
	loc := mustLondon(t)
	return threeBuckets(loc)[:2]
}

// A floor is the sum of its rooms. Energy is additive, so there is no argument
// about whether the combining statistic is meaningful — sum is the only answer,
// and no group_fn is needed.
func TestAssembleByFloorSumsItsRooms(t *testing.T) {
	buckets := twoBuckets(t)
	out := AssembleSeries(buckets, nil, floorInventory(), floorEnergy(), nil, testTariff(), GroupByFloor, nil)

	byKey := map[string]Series{}
	for _, s := range out {
		byKey[s.Key] = s
	}
	f1, ok := byKey["floor1"]
	if !ok {
		t.Fatalf("floor1 series missing: %+v", out)
	}
	// winefridge (0.05) + toaster (0.10) in bucket 0 — the meter and the
	// whole-property immersion heater are not floor1's consumption.
	if f1.KWh[0] != 0.15 || f1.KWh[1] != 0.25 {
		t.Errorf("floor1 kwh = %v, want [0.15 0.25]", f1.KWh)
	}
	if got := byKey["floor2"].KWh[0]; got != 0.30 {
		t.Errorf("floor2 kwh[0] = %v, want 0.30 (office_pc only)", got)
	}
	// A floor series belongs to no single room, so `room` stays empty rather than
	// carrying a floor id in a field named room.
	if f1.Room != "" {
		t.Errorf("floor1 room = %q, want empty", f1.Room)
	}
}

// The whole-house meter measures the same electricity as the devices on every
// floor, so including it would double-count the house — the same exclusion the
// device, room and class groupings apply.
func TestAssembleByFloorExcludesTheMeter(t *testing.T) {
	buckets := twoBuckets(t)
	out := AssembleSeries(buckets, nil, floorInventory(), floorEnergy(), nil, testTariff(), GroupByFloor, nil)
	for _, s := range out {
		if s.KWh[0] >= 5 {
			t.Errorf("series %q carries meter-sized energy %v — the meter was not excluded", s.Key, s.KWh)
		}
	}
}

// A device whose readings describe the whole property belongs to no storey, so
// it groups under the reserved "house" key rather than the floor its box hangs
// on — exactly as group_by=room already keys it.
func TestAssembleByFloorKeysWholePropertyDevicesUnderHouse(t *testing.T) {
	buckets := twoBuckets(t)
	out := AssembleSeries(buckets, nil, floorInventory(), floorEnergy(), nil, testTariff(), GroupByFloor, nil)
	byKey := map[string]Series{}
	for _, s := range out {
		byKey[s.Key] = s
	}
	house, ok := byKey["house"]
	if !ok {
		t.Fatalf("no house series: the immersion heater was dropped or misattributed: %+v", out)
	}
	if house.KWh[0] != 1 {
		t.Errorf("house kwh[0] = %v, want 1 (the immersion heater)", house.KWh[0])
	}
	if byKey["floor1"].KWh[0] != 0.15 {
		t.Errorf("floor1 kwh[0] = %v — a whole-property device was attributed to a storey", byKey["floor1"].KWh[0])
	}
}

// A device the namespace declares no floor for has an UNKNOWN floor, not a floor
// of its own: it is omitted from floor grouping rather than keyed on "" or
// derived from the "<floor>.<slug>" shape of its room id.
func TestAssembleByFloorOmitsUndeclaredFloors(t *testing.T) {
	buckets := twoBuckets(t)
	out := AssembleSeries(buckets, nil, floorInventory(), floorEnergy(), nil, testTariff(), GroupByFloor, nil)
	for _, s := range out {
		if s.Key == "" || s.Key == "floor2.room-b" {
			t.Errorf("unplaced device produced series %q; an undeclared floor is UNKNOWN", s.Key)
		}
	}
	// It must not be smuggled into floor2 either — that would be deriving the
	// floor from the room id.
	for _, s := range out {
		if s.Key == "floor2" && s.KWh[0] != 0.30 {
			t.Errorf("floor2 kwh[0] = %v, want 0.30: the unplaced device was derived onto it", s.KWh[0])
		}
	}
}

// The floorplan owns the floor: GroupKeyFor never reads it out of the room id.
func TestGroupKeyForFloorIsDeclaredNotDerived(t *testing.T) {
	keyOf := GroupKeyFor(GroupByFloor)
	if got := keyOf(config.DeviceConfig{Room: "floor1.room-a"}); got != "" {
		t.Errorf("key = %q, want empty: the floor was derived from the room id", got)
	}
	if got := keyOf(config.DeviceConfig{Room: "floor1.room-a", Floor: "floor2"}); got != "floor2" {
		t.Errorf("key = %q, want the DECLARED floor2 even though the room id says floor1", got)
	}
	if GroupKeyFor(GroupByDevice) != nil || GroupKeyFor(GroupByHouse) != nil {
		t.Error("device/house group nothing and must have no key function")
	}
}

// A grouped series is labelled with the floorplan's NAME, so a legend shows
// "Room A" to a human instead of "floor1.room-c". The KEY never changes, so a
// client matching on identity is unaffected by whether a name is published.
func TestAssembleGroupedLabelsWithFloorplanNames(t *testing.T) {
	buckets := twoBuckets(t)
	labels := map[string]string{"floor1.room-a": "Room A", "floor1": "Floor One"}

	rooms := AssembleSeries(buckets, nil, floorInventory(), floorEnergy(), nil, testTariff(), GroupByRoom, labels)
	byKey := map[string]Series{}
	for _, s := range rooms {
		byKey[s.Key] = s
	}
	a, ok := byKey["floor1.room-a"]
	if !ok {
		t.Fatalf("room-a series missing: %+v", rooms)
	}
	if a.Label != "Room A" {
		t.Errorf("label = %q, want Room A", a.Label)
	}
	// A series that IS a room reports that room, so a client can join it to the
	// /rooms catalog.
	if a.Room != "floor1.room-a" {
		t.Errorf("room = %q, want floor1.room-a", a.Room)
	}
	// No published name: the label falls back to the id rather than a label
	// derived from it — that transform is client-side guesswork, and moving it
	// here would not make it less of a guess.
	c, ok := byKey["floor1.room-c"]
	if !ok {
		t.Fatalf("room-c series missing: %+v", rooms)
	}
	if c.Label != "floor1.room-c" {
		t.Errorf("label = %q, want the id as the fallback", c.Label)
	}

	floors := AssembleSeries(buckets, nil, floorInventory(), floorEnergy(), nil, testTariff(), GroupByFloor, labels)
	for _, s := range floors {
		if s.Key == "floor1" && s.Label != "Floor One" {
			t.Errorf("floor1 label = %q, want Floor One", s.Label)
		}
		if s.Key == "floor2" && s.Label != "floor2" {
			t.Errorf("floor2 label = %q, want the id as the fallback", s.Label)
		}
	}
}

// Labels are cosmetic: an empty published name is not a label, and must not blank
// out the id a legend falls back to.
func TestAssembleGroupedIgnoresEmptyNames(t *testing.T) {
	buckets := twoBuckets(t)
	out := AssembleSeries(buckets, nil, floorInventory(), floorEnergy(), nil, testTariff(),
		GroupByRoom, map[string]string{"floor1.room-a": ""})
	for _, s := range out {
		if s.Label == "" {
			t.Errorf("series %q has an empty label", s.Key)
		}
	}
}

// Class grouping is not a place: it takes no floorplan name and no room.
func TestAssembleByClassIsUnaffectedByFloorplanNames(t *testing.T) {
	buckets := twoBuckets(t)
	out := AssembleSeries(buckets, nil, floorInventory(), floorEnergy(), nil, testTariff(),
		GroupByClass, map[string]string{"media_power_device": "Should Not Appear"})
	for _, s := range out {
		if s.Label != s.Key {
			t.Errorf("class series %q labelled %q, want the class itself", s.Key, s.Label)
		}
		if s.Room != "" {
			t.Errorf("class series %q carries room %q", s.Key, s.Room)
		}
	}
}

// The `house` key is a coverage SCOPE, not a room — it is the one key /rooms
// never lists — so the series keyed on it must not claim to be a room. Both
// openapi.yaml and README tell clients to join a room series' `room` to /rooms,
// and this is the single series for which that join is guaranteed to miss.
//
// The sibling case already reads this way: the unmonitored catch-all reports an
// empty room because it belongs to no place, and a whole-property device's series
// belongs to no place for exactly the same reason.
func TestAssembleByRoomLeavesTheHouseKeyRoomless(t *testing.T) {
	buckets := twoBuckets(t)
	out := AssembleSeries(buckets, nil, floorInventory(), floorEnergy(), nil, testTariff(), GroupByRoom, nil)

	var found bool
	for _, s := range out {
		if s.Key != houseCoverageKey {
			continue
		}
		found = true
		if s.Room != "" {
			t.Errorf("house series reports room = %q, want empty: %q is a coverage scope "+
				"that /rooms never lists, so joining it to the room catalog cannot resolve", s.Room, s.Room)
		}
	}
	if !found {
		t.Fatalf("no house series to check: %+v", out)
	}
}

// Same guard, second half: a floorplan that published a room record with the
// reserved id would otherwise relabel this series with a name for a group /rooms
// does not list. Vanishingly unlikely, and one condition covers both.
func TestAssembleGroupedNeverRelabelsTheHouseKey(t *testing.T) {
	buckets := twoBuckets(t)
	labels := map[string]string{houseCoverageKey: "The Whole House"}

	for _, groupBy := range []string{GroupByRoom, GroupByFloor} {
		out := AssembleSeries(buckets, nil, floorInventory(), floorEnergy(), nil, testTariff(), groupBy, labels)
		for _, s := range out {
			if s.Key == houseCoverageKey && s.Label != houseCoverageKey {
				t.Errorf("group_by=%s: house series labelled %q, want the reserved key itself",
					groupBy, s.Label)
			}
		}
	}
}
