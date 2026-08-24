package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sweeney/countinghouse/internal/config"
)

// fakeFloorplan is a static FloorplanProvider for handler tests.
type fakeFloorplan struct {
	floors map[string]config.FloorConfig
	rooms  map[string]config.RoomConfig
}

func (f fakeFloorplan) Floors() map[string]config.FloorConfig { return f.floors }
func (f fakeFloorplan) Rooms() map[string]config.RoomConfig   { return f.rooms }

func intPtr(v int) *int           { return &v }
func floatPtr(v float64) *float64 { return &v }

// floorplanDevices is the inventory the floorplan tests share: two metered
// devices on floor1, one on floor2, one UNMETERED sensor (countinghouse charges
// for energy, so it is not part of any catalog), the whole-house meter (excluded
// from every fleet grouping — it measures the others), and a whole-property
// device (grouped under the reserved "house" key, which is not a room).
func floorplanDevices() map[string]config.DeviceConfig {
	return map[string]config.DeviceConfig{
		"winefridge": {
			Class: "continuous_power_device", DisplayName: "Wine Fridge",
			Room: "floor1.room-a", Floor: "floor1",
		},
		"washingmachine": {
			Class: "cycle_power_device", DisplayName: "Washing Machine",
			Room: "floor1.room-a", Floor: "floor1",
		},
		"network-ups": {
			Class: "ups_sensor", DisplayName: "Network UPS",
			Room: "floor2.room-b", Floor: "floor2",
		},
		"hallway-sensor": {
			Class: "environmental_sensor", DisplayName: "Hall Sensor",
			Room: "floor1.room-c", Floor: "floor1",
		},
		"electricity_meter": {
			Class: "energy_meter", DisplayName: "Electricity Meter",
			Room: "floor1.room-c", Floor: "floor1", Covers: "house",
		},
		"immersion": {
			Class: "binary_state_device", DisplayName: "Immersion Heater",
			Room: "floor1.room-c", Floor: "floor1", Covers: "house",
		},
	}
}

func floorplanRecords() fakeFloorplan {
	return fakeFloorplan{
		floors: map[string]config.FloorConfig{
			"floor1": {ID: "floor1", Name: "Floor One", Order: intPtr(1), Elevation: floatPtr(0.0)},
			"floor2": {ID: "floor2", Name: "Floor Two", Order: intPtr(2), Elevation: floatPtr(2.8)},
			// A floor nothing metered sits on: real in the building, absent from
			// the energy API.
			"floor3": {ID: "floor3", Name: "Attic", Order: intPtr(3)},
		},
		rooms: map[string]config.RoomConfig{
			"floor1.room-a": {ID: "floor1.room-a", Name: "Room A", Floor: "floor1", Category: "utility", Area: floatPtr(12.4)},
			"floor1.room-c": {ID: "floor1.room-c", Name: "Room C", Floor: "floor1", Category: "plant", Area: floatPtr(1.3)},
			"floor2.room-b": {ID: "floor2.room-b", Name: "Room B", Floor: "floor2", Category: "bedroom"},
		},
	}
}

// floorplanSetup wires a Server with the shared inventory and, unless nil is
// wanted, the floorplan records.
func floorplanSetup(t *testing.T, fp *fakeFloorplan) *Server {
	t.Helper()
	s := setup(t)
	s.Config = fakeConfig{devices: floorplanDevices(), tariffs: testTariffs()}
	if fp != nil {
		s.Floorplan = *fp
	}
	return s
}

func getJSON(t *testing.T, s *Server, path string) (int, map[string]any) {
	t.Helper()
	mux := newMux(s)
	r := httptest.NewRequest(http.MethodGet, path, nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal %s: %v (body %s)", path, err, w.Body.String())
	}
	return w.Code, body
}

// entries pulls a list-of-objects field out of a decoded response.
func entries(t *testing.T, body map[string]any, key string) []map[string]any {
	t.Helper()
	raw, ok := body[key].([]any)
	if !ok {
		t.Fatalf("%q missing or not a list: %v", key, body)
	}
	out := make([]map[string]any, 0, len(raw))
	for _, e := range raw {
		m, ok := e.(map[string]any)
		if !ok {
			t.Fatalf("%q entry is not an object: %v", key, e)
		}
		out = append(out, m)
	}
	return out
}

// The devices namespace declares `floor` as a first-class property alongside
// `room`, so the catalog relays it. A client organising devices by storey must
// not have to split the room id on its first dot and hope the convention holds.
func TestDevices_CarriesDeclaredFloor(t *testing.T) {
	s := floorplanSetup(t, nil)
	code, body := getJSON(t, s, "/devices")
	if code != http.StatusOK {
		t.Fatalf("want 200, got %d", code)
	}
	byID := map[string]map[string]any{}
	for _, e := range entries(t, body, "devices") {
		byID[e["id"].(string)] = e
	}
	if got := byID["winefridge"]["floor"]; got != "floor1" {
		t.Errorf("winefridge floor = %v, want floor1", got)
	}
	if got := byID["network-ups"]["floor"]; got != "floor2" {
		t.Errorf("network-ups floor = %v, want floor2", got)
	}
}

// An undeclared floor is UNKNOWN, reported empty. Deriving it from the room id
// would be a second implementation of the floorplan's taxonomy.
func TestDevices_UndeclaredFloorIsEmptyNotDerived(t *testing.T) {
	s := setup(t)
	s.Config = fakeConfig{
		devices: map[string]config.DeviceConfig{
			"winefridge": {Class: "continuous_power_device", Room: "floor1.room-a"},
		},
		tariffs: testTariffs(),
	}
	_, body := getJSON(t, s, "/devices")
	e := entries(t, body, "devices")[0]
	if got, ok := e["floor"]; !ok || got != "" {
		t.Errorf("floor = %v (present=%v), want an empty string, never derived from floor1.room-a", got, ok)
	}
}

// /floors lists exactly the floors that hold a device this service bills for —
// the same set floors= accepts and group_by=floor produces, so a picker filled
// from this endpoint cannot build a request the API rejects.
func TestFloors_ListsFloorsHoldingMeteredDevices(t *testing.T) {
	fp := floorplanRecords()
	s := floorplanSetup(t, &fp)
	code, body := getJSON(t, s, "/floors")
	if code != http.StatusOK {
		t.Fatalf("want 200, got %d", code)
	}
	got := entries(t, body, "floors")
	if len(got) != 2 {
		t.Fatalf("floors = %v, want floor1 and floor2 only", got)
	}
	// Declared storey order, ascending.
	if got[0]["id"] != "floor1" || got[1]["id"] != "floor2" {
		t.Errorf("order = %v/%v, want floor1 then floor2", got[0]["id"], got[1]["id"])
	}
	if got[0]["name"] != "Floor One" {
		t.Errorf("floor1 name = %v, want Floor One", got[0]["name"])
	}
	if got[0]["order"] != float64(1) || got[0]["elevation"] != float64(0) {
		t.Errorf("floor1 order/elevation = %v/%v, want 1/0", got[0]["order"], got[0]["elevation"])
	}
	// winefridge + washingmachine. The unmetered sensor, the meter and the
	// whole-property immersion heater are not floor1's device_count: the first is
	// not billed at all, and the other two are not attributed to a floor.
	if got[0]["device_count"] != float64(2) {
		t.Errorf("floor1 device_count = %v, want 2", got[0]["device_count"])
	}
	if got[1]["device_count"] != float64(1) {
		t.Errorf("floor2 device_count = %v, want 1", got[1]["device_count"])
	}
}

// A floor the floorplan records but no metered device declares is not listed: it
// exists in the building, but not as far as the energy API is concerned.
func TestFloors_OmitsFloorsWithNoMeteredDevice(t *testing.T) {
	fp := floorplanRecords()
	s := floorplanSetup(t, &fp)
	_, body := getJSON(t, s, "/floors")
	for _, e := range entries(t, body, "floors") {
		if e["id"] == "floor3" {
			t.Errorf("floor3 holds no metered device but was listed: %v", e)
		}
	}
}

// The mirror case: a floor devices declare that the floorplan has no record for
// IS listed, with its name empty and order null. Countinghouse reports UNKNOWN
// rather than title-casing an id or inventing a position.
func TestFloors_UnknownRecordReportsNullOrderAndEmptyName(t *testing.T) {
	fp := fakeFloorplan{floors: map[string]config.FloorConfig{
		"floor1": {ID: "floor1", Name: "Floor One", Order: intPtr(1)},
	}}
	s := floorplanSetup(t, &fp)
	_, body := getJSON(t, s, "/floors")
	got := entries(t, body, "floors")
	if len(got) != 2 {
		t.Fatalf("floors = %v, want floor1 and floor2", got)
	}
	// floor1 has an order, floor2 does not: undeclared sorts last.
	if got[1]["id"] != "floor2" {
		t.Fatalf("want floor2 sorted last, got %v", got)
	}
	if got[1]["name"] != "" {
		t.Errorf("floor2 name = %v, want empty", got[1]["name"])
	}
	if got[1]["order"] != nil || got[1]["elevation"] != nil {
		t.Errorf("floor2 order/elevation = %v/%v, want null (undeclared, not zero)",
			got[1]["order"], got[1]["elevation"])
	}
}

// With no floorplan namespace configured the catalog still answers: every floor
// that holds a metered device, with every label unknown. A missing floorplan is
// a missing label, not a missing endpoint.
func TestFloors_NoFloorplanConfigured(t *testing.T) {
	s := floorplanSetup(t, nil)
	code, body := getJSON(t, s, "/floors")
	if code != http.StatusOK {
		t.Fatalf("want 200, got %d", code)
	}
	got := entries(t, body, "floors")
	if len(got) != 2 {
		t.Fatalf("floors = %v, want floor1 and floor2", got)
	}
	for _, e := range got {
		if e["name"] != "" || e["order"] != nil {
			t.Errorf("%v: want name empty and order null with no floorplan", e)
		}
	}
	// Deterministic without any declared order: by id.
	if got[0]["id"] != "floor1" || got[1]["id"] != "floor2" {
		t.Errorf("order = %v, want id order as the tiebreak", got)
	}
}

// The reserved "house" key is a coverage scope, not a floor: a whole-property
// device belongs to no storey, and listing "house" would advertise a floor
// floors= must reject.
func TestFloors_NeverListsTheReservedHouseKey(t *testing.T) {
	fp := floorplanRecords()
	s := floorplanSetup(t, &fp)
	_, body := getJSON(t, s, "/floors")
	for _, e := range entries(t, body, "floors") {
		if e["id"] == "house" {
			t.Errorf("the reserved house key was listed as a floor: %v", e)
		}
	}
}

// /rooms is the room-shaped sibling of /floors, and matters for energy
// specifically: `category` is what lets a client separate a plant room's
// infrastructure load from household usage.
func TestRooms_ListsRoomsHoldingMeteredDevices(t *testing.T) {
	fp := floorplanRecords()
	s := floorplanSetup(t, &fp)
	code, body := getJSON(t, s, "/rooms")
	if code != http.StatusOK {
		t.Fatalf("want 200, got %d", code)
	}
	got := entries(t, body, "rooms")
	if len(got) != 2 {
		t.Fatalf("rooms = %v, want floor1.room-a and floor2.room-b only", got)
	}
	a := got[0]
	if a["id"] != "floor1.room-a" || a["name"] != "Room A" || a["floor"] != "floor1" {
		t.Errorf("room-a = %v, want the floorplan's id/name/floor relayed", a)
	}
	if a["category"] != "utility" {
		t.Errorf("room-a category = %v, want the raw category relayed", a["category"])
	}
	if a["area"] != 12.4 {
		t.Errorf("room-a area = %v, want 12.4", a["area"])
	}
	if a["device_count"] != float64(2) {
		t.Errorf("room-a device_count = %v, want 2", a["device_count"])
	}
	// room-c holds only the unmetered sensor, the meter and the whole-property
	// immersion heater — nothing group_by=room bills to it — so it is absent
	// despite having a floorplan record.
	if got[1]["id"] != "floor2.room-b" {
		t.Errorf("rooms = %v, want room-c omitted (no metered device attributed to it)", got)
	}
	// Undeclared area is null, never 0.
	if got[1]["area"] != nil {
		t.Errorf("room-b area = %v, want null", got[1]["area"])
	}
}

// Rooms come back in building order — floor storey order, then room id — so a
// client renders the list top to bottom without re-sorting. A room whose floor
// is unknown sorts last rather than first.
func TestRooms_SortedByFloorOrderThenID(t *testing.T) {
	fp := fakeFloorplan{
		floors: map[string]config.FloorConfig{
			"floor1": {ID: "floor1", Order: intPtr(2)},
			"floor2": {ID: "floor2", Order: intPtr(1)},
		},
		rooms: map[string]config.RoomConfig{
			"floor1.room-a": {ID: "floor1.room-a", Floor: "floor1"},
			"floor2.room-b": {ID: "floor2.room-b", Floor: "floor2"},
		},
	}
	s := floorplanSetup(t, &fp)
	_, body := getJSON(t, s, "/rooms")
	got := entries(t, body, "rooms")
	if got[0]["id"] != "floor2.room-b" || got[1]["id"] != "floor1.room-a" {
		t.Errorf("order = %v, want floor2's room first (storey order 1)", got)
	}
}

// A room devices sit in that the floorplan has no record for is still listed,
// with name, floor and category empty and area null.
func TestRooms_UnknownRecordReportsEmptyFields(t *testing.T) {
	s := floorplanSetup(t, nil)
	code, body := getJSON(t, s, "/rooms")
	if code != http.StatusOK {
		t.Fatalf("want 200, got %d", code)
	}
	got := entries(t, body, "rooms")
	if len(got) != 2 {
		t.Fatalf("rooms = %v, want both rooms holding metered devices", got)
	}
	for _, e := range got {
		if e["name"] != "" || e["floor"] != "" || e["category"] != "" || e["area"] != nil {
			t.Errorf("%v: want every floorplan field unknown with no floorplan configured", e)
		}
	}
}

// "house" is a coverage scope and a reserved series key, never a room id.
func TestRooms_NeverListsTheReservedHouseKey(t *testing.T) {
	fp := floorplanRecords()
	s := floorplanSetup(t, &fp)
	_, body := getJSON(t, s, "/rooms")
	for _, e := range entries(t, body, "rooms") {
		if e["id"] == "house" {
			t.Errorf("the reserved house key was listed as a room: %v", e)
		}
	}
}
