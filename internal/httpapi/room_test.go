package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/sweeney/countinghouse/internal/config"
)

// The floorplan migration renames the read-side vocabulary: `location` conflated a
// geographic site with a room, so rooms are now `room`, keyed on floorplan ids of the
// form <floor>.<slug>. `location` stays as a deprecated alias for one release.

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

// TestGroupByRoomAndLocationAreEquivalent is the alias-period contract: the two
// spellings return the same numbers, and only the reported group_by differs.
func TestGroupByRoomAndLocationAreEquivalent(t *testing.T) {
	s, _ := dataSetup(t)
	s.Config = fakeConfig{devices: roomDevices(), tariffs: testTariffs()}

	byRoom := doGET(t, s, "/series?window=today&interval=1h&group_by=room")
	byLocation := doGET(t, s, "/series?window=today&interval=1h&group_by=location")
	if byRoom.Code != http.StatusOK || byLocation.Code != http.StatusOK {
		t.Fatalf("codes: room=%d location=%d", byRoom.Code, byLocation.Code)
	}

	var a, b map[string]any
	if err := json.Unmarshal(byRoom.Body.Bytes(), &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(byLocation.Body.Bytes(), &b); err != nil {
		t.Fatal(err)
	}
	if a["group_by"] != "room" || b["group_by"] != "location" {
		t.Errorf("group_by: %v and %v", a["group_by"], b["group_by"])
	}
	delete(a, "group_by")
	delete(b, "group_by")
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	if string(ja) != string(jb) {
		t.Errorf("the alias returns different data\n room: %s\n  loc: %s", ja, jb)
	}
}

// The /bill breakdown carries a place per device, and must carry the room id.
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
	if !strings.Contains(w.Body.String(), `"location":"groundfloor.kitchen"`) {
		t.Errorf("bill breakdown dropped the deprecated alias: %s", w.Body.String())
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
