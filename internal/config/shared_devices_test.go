package config

import (
	"context"
	"net/http"
	"testing"
)

// The devices namespace is read with the shared schema (common/devices), field
// by field: a value that does not fit costs that field, not the snapshot.
// Before, one bad value anywhere made the whole document fail to decode, and
// countinghouse kept its last-known snapshot, missing every later change.
func TestFetcher_BadValueCostsOnlyThatField(t *testing.T) {
	mux := http.NewServeMux()
	serveNamespace(mux, "devices_home", map[string]any{
		"kettle": map[string]any{"class": "short_burst_power_device", "room": "ground.kitchen"},
		"heater": map[string]any{
			"class": "cycle_power_device",
			"room":  "ground.office",
			"floor": 7,
		},
	})
	serveNamespace(mux, "energy_tariffs", map[string]any{"tariffs": map[string]any{}})

	f := newTestFetcher(t, mux, &staticTokenSource{token: "test-token"})
	f.Refresh(context.Background())

	if st := f.Statuses()["devices_home"]; !st.OK {
		t.Fatalf("devices_home status = %+v, want ok", st)
	}
	got := f.Devices()
	if got["kettle"].Room != "ground.kitchen" {
		t.Errorf("kettle = %+v", got["kettle"])
	}
	if h := got["heater"]; h.Class != "cycle_power_device" || h.Room != "ground.office" || h.Floor != "" {
		t.Errorf("heater lost more than its bad field: %+v", h)
	}
}

// Coverage follows statehouse: a legacy `location: house` still means the
// whole site once `room` is published, because a namespace mid-migration may
// publish room before covers. countinghouse used to drop the coverage fact in
// that window and bill the meter to a room.
func TestCoversWholeSite_RoomPublishedFirst(t *testing.T) {
	d := DeviceConfig{Class: "energy_meter", Room: "ground.hall", Location: CoverageHouse}
	if !d.CoversWholeSite() {
		t.Error("room + legacy location house: CoversWholeSite = false, want true (statehouse's rule)")
	}
	if d.Place() != "ground.hall" {
		t.Errorf("Place = %q, want the room", d.Place())
	}
}
