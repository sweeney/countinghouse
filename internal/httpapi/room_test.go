package httpapi

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/sweeney/countinghouse/internal/config"
	"github.com/sweeney/countinghouse/internal/energy"
	"github.com/sweeney/countinghouse/internal/influx"
)

// The floorplan migration renames the read-side vocabulary: `location` conflated a
// geographic site with a room, so rooms are now `room`, keyed on floorplan ids of the
// form <floor>.<slug>. The deprecated `location` spelling has been removed.

func roomDevices() map[string]config.DeviceConfig {
	return map[string]config.DeviceConfig{
		"winefridge": {
			Class: "continuous_power_device", Room: "groundfloor.kitchen", DisplayName: "Wine Fridge",
		},
		"network-ups": {
			Class: "ups_sensor", Room: "basement.network-cabinet", DisplayName: "Network UPS",
		},
		"electricity_meter": {
			Class: "energy_meter", Room: "basement.hallway", Covers: "house",
			DisplayName: "Electricity Meter",
		},
		"hot_water": {
			Class: "binary_state_device", Room: "groundfloor.boiler-room", Covers: "house",
			DisplayName: "Hot Water",
		},
	}
}

func TestSeries_GroupByRoomKeysOnRoomIDs(t *testing.T) {
	s, _ := dataSetup(t)
	s.Config = fakeConfig{devices: roomDevices(), tariffs: testTariffs()}

	w := doGET(t, s, "/series?window=today&interval=1h&group_by=room")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		GroupBy string `json:"group_by"`
		Series  []struct {
			Key string `json:"key"`
		} `json:"series"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.GroupBy != "room" {
		t.Errorf("group_by = %q, want %q", resp.GroupBy, "room")
	}
	var keys []string
	for _, ser := range resp.Series {
		keys = append(keys, ser.Key)
	}
	if !hasKey(keys, "groundfloor.kitchen") {
		t.Errorf("no series keyed on a room id: %v", keys)
	}
}

func TestBillBreakdownCarriesRoom(t *testing.T) {
	s, _ := dataSetup(t)
	s.Config = fakeConfig{devices: roomDevices(), tariffs: testTariffs()}

	w := doGET(t, s, "/bill?window=today")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"room":"groundfloor.kitchen"`) {
		t.Errorf("bill breakdown has no room id: %s", w.Body.String())
	}
}

// A device covering the whole house sits in a room but reports for the property. That
// distinction is the one structural defect in the old single-field scheme, so the two
// facts must survive as two fields.
func TestCoversIsCarriedSeparatelyFromRoom(t *testing.T) {
	devs := roomDevices()
	if devs["electricity_meter"].Covers != "house" {
		t.Fatal("fixture is wrong")
	}
	d := devs["electricity_meter"]
	if d.Place() != "basement.hallway" {
		t.Errorf("Place() = %q, want the room it sits in, not its coverage", d.Place())
	}
	if !d.CoversWholeSite() {
		t.Error("CoversWholeSite() = false for a device declaring covers: house")
	}
	if devs["winefridge"].CoversWholeSite() {
		t.Error("a device with no covers must cover only its own room")
	}
}

// A namespace still declaring `location` must keep working untouched.
func TestLegacyLocationOnlyConfigStillGroups(t *testing.T) {
	s, _ := dataSetup(t)

	w := doGET(t, s, "/series?window=today&interval=1h&group_by=room")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"key":"kitchen"`) {
		t.Errorf("a location-only namespace must still group: %s", w.Body.String())
	}
}

func hasKey(keys []string, want string) bool {
	for _, k := range keys {
		if k == want {
			return true
		}
	}
	return false
}

// GET /devices is a second catalog path, in events_handlers.go rather than
// handlers.go, and it was missed when rooms were added: it read dev.Location
// directly, so the openapi spec and README promised a `room` the code never sent.
//
// It matters more than a missing field. Once the devices namespace is republished
// it carries `room` and stops carrying `location`, so this endpoint would have
// returned an empty location for every device — and the demo dashboards label
// their device pickers from exactly this response.
func TestDevicesCatalogCarriesRoom(t *testing.T) {
	s, _ := dataSetup(t)
	s.Config = fakeConfig{devices: roomDevices(), tariffs: testTariffs()}

	w := doGET(t, s, "/devices")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		Devices []struct {
			ID       string `json:"id"`
			Room     string `json:"room"`
			Location string `json:"location"`
		} `json:"devices"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	var seen bool
	for _, d := range resp.Devices {
		if d.ID != "winefridge" {
			continue
		}
		seen = true
		if d.Room != "groundfloor.kitchen" {
			t.Errorf("room = %q, want groundfloor.kitchen", d.Room)
		}
		if d.Location != "" {
			t.Errorf("the deprecated location alias is still emitted (%q)", d.Location)
		}
	}
	if !seen {
		t.Fatalf("winefridge missing from the catalog: %s", w.Body.String())
	}
}

// A namespace still declaring the free-text location must keep populating `room`:
// the API alias is retired here, the config field is not.
func TestDevicesCatalogFallsBackToLegacyLocation(t *testing.T) {
	s, _ := dataSetup(t)

	w := doGET(t, s, "/devices")
	if !strings.Contains(w.Body.String(), `"room":"kitchen"`) {
		t.Errorf("a location-only namespace must still populate room: %s", w.Body.String())
	}
	if strings.Contains(w.Body.String(), `"location"`) {
		t.Errorf("the deprecated location alias is still emitted: %s", w.Body.String())
	}
}

// A whole-property device has no room, and saying so with an empty string loses the
// reason. The catalog and the bill therefore carry `covers`, so a consumer can tell
// "no room known" from "not attributable to a room" — the same two-facts-two-fields
// split the rest of this migration applies.
func TestCatalogReportsCoverageForWholePropertyDevices(t *testing.T) {
	s, _ := dataSetup(t)
	s.Config = fakeConfig{devices: map[string]config.DeviceConfig{
		"immersion": {Class: "cycle_power_device", DisplayName: "Immersion", Location: "house"},
		"fridge":    {Class: "continuous_power_device", DisplayName: "Fridge", Room: "groundfloor.kitchen"},
	}, tariffs: testTariffs()}

	var resp struct {
		Devices []struct {
			ID     string `json:"id"`
			Room   string `json:"room"`
			Covers string `json:"covers"`
		} `json:"devices"`
	}
	if err := json.Unmarshal(doGET(t, s, "/devices").Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}

	for _, d := range resp.Devices {
		switch d.ID {
		case "immersion":
			if d.Covers != "house" {
				t.Errorf("immersion covers = %q, want house", d.Covers)
			}
		case "fridge":
			if d.Covers != "" {
				t.Errorf("fridge covers = %q, want empty", d.Covers)
			}
			if d.Room != "groundfloor.kitchen" {
				t.Errorf("fridge room = %q", d.Room)
			}
		}
	}
}

// billableFixture returns a device set and the matching querier responses, sized so
// an unsorted breakdown cannot pass by luck.
//
// The ids are deliberately not in their sorted order here, and there are nine of
// them: ranging the map unsorted lands on sorted output with probability 1/9!, about
// three in a million per request, against 1/2 for the two-billable fixture this test
// used to run on. A test that a broken implementation passes half the time is not a
// test. The names are also chosen so sorted order differs from any plausible
// insertion or grouping order.
func billableFixture() (map[string]config.DeviceConfig, map[string][]influx.Row) {
	ids := []string{
		"winefridge", "boiler-pump", "network-ups", "aquarium", "tumble-dryer",
		"server-rack", "dehumidifier", "immersion", "car-charger",
	}
	devices := make(map[string]config.DeviceConfig, len(ids))
	responses := make(map[string][]influx.Row, len(ids))
	for i, id := range ids {
		devices[id] = config.DeviceConfig{
			Class:       "continuous_power_device",
			Room:        "groundfloor.kitchen",
			DisplayName: id,
		}
		// Distinct values so nothing can accidentally order by consumption.
		responses[`r.device_id == "`+id+`"`] = []influx.Row{{Value: float64(i+1) * 1.5}}
	}
	return devices, responses
}

// The /bill breakdown was built by ranging a map and never sorted, so the device
// order was different on every request. Clients diffing two bills, or rendering a
// table, saw spurious churn — and it made the golden snapshot pass locally and fail
// on CI purely on Go's randomised map iteration.
func TestBillBreakdownIsOrderedByDeviceID(t *testing.T) {
	s, q := dataSetup(t)
	devices, responses := billableFixture()
	q.Responses = responses
	s.Config = fakeConfig{devices: devices, tariffs: testTariffs()}

	var previous []string
	for i := 0; i < 8; i++ {
		var resp struct {
			Devices []struct {
				DeviceID string `json:"device_id"`
			} `json:"devices"`
		}
		if err := json.Unmarshal(doGET(t, s, "/bill?window=today").Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, d := range resp.Devices {
			ids = append(ids, d.DeviceID)
		}
		if len(ids) < 9 {
			t.Fatalf("fixture no longer produces 9 billable devices, got %d: %v — "+
				"the assertion below is only meaningful at that width", len(ids), ids)
		}
		if !sort.StringsAreSorted(ids) {
			t.Fatalf("breakdown is not ordered by device id: %v", ids)
		}
		if previous != nil && strings.Join(ids, ",") != strings.Join(previous, ",") {
			t.Fatalf("breakdown order varies between requests: %v then %v", previous, ids)
		}
		previous = ids
	}
}

// The removal is the point of this change, and nothing asserted it: the only two
// places the spelling appeared were deleted with it, and TestSeries_BadGroupBy only
// tries a nonsense value. Re-adding GroupByLocation to validGroupBy would have left
// the whole suite green.
func TestGroupByLocationIsRejected(t *testing.T) {
	s, _ := dataSetup(t)
	s.Config = fakeConfig{devices: roomDevices(), tariffs: testTariffs()}

	w := doGET(t, s, "/series?window=today&group_by=location")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", w.Code, w.Body.String())
	}
}

// /devices gained `covers` so an empty room is legible. The bill is billed per device
// and the whole-property devices are metered classes, so they appear in the breakdown
// with room "" — and without covers there is nothing to distinguish "no room
// configured" from "meters the whole property".
func TestBillBreakdownCarriesCoverage(t *testing.T) {
	s, _ := dataSetup(t)
	s.Config = fakeConfig{devices: map[string]config.DeviceConfig{
		"immersion":         {Class: "cycle_power_device", DisplayName: "Immersion", Location: "house"},
		"winefridge":        {Class: "continuous_power_device", DisplayName: "Wine Fridge", Room: "groundfloor.kitchen"},
		"electricity_meter": {Class: energy.EnergyMeterClass, DisplayName: "Meter", Room: "basement.hallway"},
	}, tariffs: testTariffs()}

	var resp struct {
		Devices []struct {
			DeviceID string `json:"device_id"`
			Room     string `json:"room"`
			Covers   string `json:"covers"`
		} `json:"devices"`
	}
	if err := json.Unmarshal(doGET(t, s, "/bill?window=today").Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}

	var seen bool
	for _, d := range resp.Devices {
		if d.DeviceID != "immersion" {
			continue
		}
		seen = true
		if d.Covers != "house" {
			t.Errorf("immersion covers = %q, want house — an empty room is otherwise unexplained", d.Covers)
		}
	}
	if !seen {
		t.Fatal("immersion missing from the breakdown")
	}
}
