package config

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// The floorplan namespace is the same document greenhouse reads (issue #19), so
// these tests pin the same two accepted shapes: the published wrapper of ARRAYS,
// and the legacy devices-style map keyed by id (floors only).
func TestFloorplanDocument_WrapperShape(t *testing.T) {
	var doc floorplanDocument
	raw := `{
	  "floors": [
	    {"id": "floor1", "name": "Floor One", "order": 1, "elevation": 0.0},
	    {"id": "floor2", "name": "Floor Two", "order": 2, "elevation": 2.8}
	  ],
	  "rooms": [
	    {"id": "floor1.room-a", "name": "Room A", "floor": "floor1", "category": "utility", "area": 12.4},
	    {"id": "floor1.room-c", "name": "Room C", "floor": "floor1", "category": "plant"}
	  ],
	  "ceiling": {"unmodelled": true}
	}`
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	f1, ok := doc.Floors["floor1"]
	if !ok {
		t.Fatal("floor1 missing")
	}
	if f1.Name != "Floor One" {
		t.Errorf("floor1 name = %q, want Floor One", f1.Name)
	}
	if f1.Order == nil || *f1.Order != 1 {
		t.Errorf("floor1 order = %v, want 1", f1.Order)
	}
	if f1.Elevation == nil || *f1.Elevation != 0.0 {
		t.Errorf("floor1 elevation = %v, want 0", f1.Elevation)
	}

	roomA, ok := doc.Rooms["floor1.room-a"]
	if !ok {
		t.Fatal("floor1.room-a missing")
	}
	if roomA.Name != "Room A" || roomA.Floor != "floor1" || roomA.Category != "utility" {
		t.Errorf("room-a = %+v, want name/floor/category relayed", roomA)
	}
	if roomA.Area == nil || *roomA.Area != 12.4 {
		t.Errorf("room-a area = %v, want 12.4", roomA.Area)
	}
	// An undeclared area is UNKNOWN, not zero.
	if doc.Rooms["floor1.room-c"].Area != nil {
		t.Errorf("room-c area = %v, want nil", doc.Rooms["floor1.room-c"].Area)
	}
}

// The legacy map shape carries floors only: it predates rooms being published,
// so every room is honestly unknown rather than guessed.
func TestFloorplanDocument_LegacyMapShape(t *testing.T) {
	var doc floorplanDocument
	raw := `{"floor1": {"name": "Floor One", "order": 1}, "floor2": {"name": "Floor Two"}}`
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(doc.Floors) != 2 {
		t.Fatalf("floors = %d, want 2", len(doc.Floors))
	}
	if doc.Floors["floor1"].Name != "Floor One" {
		t.Errorf("floor1 name = %q", doc.Floors["floor1"].Name)
	}
	// Absence is meaningful: floor2 declares no order, which must not read as 0.
	if doc.Floors["floor2"].Order != nil {
		t.Errorf("floor2 order = %v, want nil", doc.Floors["floor2"].Order)
	}
	if len(doc.Rooms) != 0 {
		t.Errorf("rooms = %v, want none from the legacy shape", doc.Rooms)
	}
}

// A record with no id cannot be referenced by a device's room/floor property or
// matched by rooms=/floors=, so it is unusable rather than merely unlabelled.
func TestFloorplanDocument_SkipsIDLessRecords(t *testing.T) {
	var doc floorplanDocument
	raw := `{"floors": [{"name": "Nameless"}, {"id": "floor1"}],
	         "rooms":  [{"name": "Nowhere"}, {"id": "floor1.room-a"}]}`
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(doc.Floors) != 1 || len(doc.Rooms) != 1 {
		t.Fatalf("floors=%v rooms=%v, want only the id-bearing records", doc.Floors, doc.Rooms)
	}
	if _, ok := doc.Floors[""]; ok {
		t.Error("an empty key was invented for an id-less floor")
	}
}

// The map KEY is authoritative: it is what devices reference and what the
// filters match, so a record whose inner id disagrees cannot be allowed to win.
func TestNormaliseFloorsAndRooms_KeyWins(t *testing.T) {
	floors := map[string]FloorConfig{"floor1": {ID: "wrong"}}
	normaliseFloors(floors)
	if floors["floor1"].ID != "floor1" {
		t.Errorf("floor id = %q, want floor1", floors["floor1"].ID)
	}
	rooms := map[string]RoomConfig{"floor1.room-a": {ID: "wrong"}}
	normaliseRooms(rooms)
	if rooms["floor1.room-a"].ID != "floor1.room-a" {
		t.Errorf("room id = %q, want floor1.room-a", rooms["floor1.room-a"].ID)
	}
}

func TestFetcher_RefreshPopulatesFloorplan(t *testing.T) {
	mux := http.NewServeMux()
	serveNamespace(mux, "devices_home", map[string]any{})
	serveNamespace(mux, "energy_tariffs", map[string]any{})
	serveNamespace(mux, "floorplan_home", map[string]any{
		"floors": []any{map[string]any{"id": "floor1", "name": "Floor One", "order": 1}},
		"rooms": []any{map[string]any{
			"id": "floor1.room-a", "name": "Room A", "floor": "floor1", "category": "utility",
		}},
	})

	f := newTestFetcher(t, mux, &staticTokenSource{token: "test-token"})
	f.FloorplanNamespace = "floorplan_home"
	f.Refresh(context.Background())

	if got := f.Floors()["floor1"].Name; got != "Floor One" {
		t.Errorf("floor name = %q, want Floor One", got)
	}
	if got := f.Rooms()["floor1.room-a"].Name; got != "Room A" {
		t.Errorf("room name = %q, want Room A", got)
	}
	st, ok := f.Statuses()["floorplan_home"]
	if !ok || !st.OK {
		t.Errorf("floorplan status = %+v, want a recorded success", st)
	}
}

// The floorplan is presentation detail — names, storey order, category — so an
// unset namespace is silent and records no status. Countinghouse bills energy;
// a missing floorplan must never look like a fault.
func TestFetcher_FloorplanNamespaceOptional(t *testing.T) {
	mux := http.NewServeMux()
	serveNamespace(mux, "devices_home", map[string]any{})
	serveNamespace(mux, "energy_tariffs", map[string]any{})

	f := newTestFetcher(t, mux, &staticTokenSource{token: "test-token"})
	f.Refresh(context.Background())

	if len(f.Floors()) != 0 || len(f.Rooms()) != 0 {
		t.Errorf("floors=%v rooms=%v, want empty with no namespace configured", f.Floors(), f.Rooms())
	}
	for ns := range f.Statuses() {
		if ns != "devices_home" && ns != "energy_tariffs" {
			t.Errorf("unexpected status for %q: nothing was configured to fetch", ns)
		}
	}
}

// Fail-open, like every other namespace: a configured-but-failing floorplan
// keeps the last-known snapshot and records the failure for the operator.
func TestFetcher_FloorplanFailureKeepsLastKnown(t *testing.T) {
	mux := http.NewServeMux()
	serveNamespace(mux, "devices_home", map[string]any{})
	serveNamespace(mux, "energy_tariffs", map[string]any{})
	serve := true
	mux.HandleFunc("/api/v1/config/floorplan_home", func(w http.ResponseWriter, _ *http.Request) {
		if !serve {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
			"floors": []any{map[string]any{"id": "floor1", "name": "Floor One"}},
		})
	})

	f := newTestFetcher(t, mux, &staticTokenSource{token: "test-token"})
	f.FloorplanNamespace = "floorplan_home"
	f.Refresh(context.Background())
	serve = false
	f.Refresh(context.Background())

	if got := f.Floors()["floor1"].Name; got != "Floor One" {
		t.Errorf("floor name = %q, want the last-known Floor One", got)
	}
	if st := f.Statuses()["floorplan_home"]; st.OK || st.Error == "" {
		t.Errorf("floorplan status = %+v, want a recorded failure", st)
	}
}

// A device's floor is DECLARED by the namespace, never derived from the room id:
// the floorplan owns that fact, and re-deriving it here would be a second
// implementation of someone else's taxonomy.
func TestDeviceConfig_FloorIsDecoded(t *testing.T) {
	var d DeviceConfig
	if err := json.Unmarshal([]byte(`{"room": "floor2.room-a", "floor": "floor2"}`), &d); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if d.Floor != "floor2" {
		t.Errorf("floor = %q, want floor2", d.Floor)
	}
	var undeclared DeviceConfig
	if err := json.Unmarshal([]byte(`{"room": "floor2.room-a"}`), &undeclared); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if undeclared.Floor != "" {
		t.Errorf("floor = %q, want empty (UNKNOWN, not derived from the room id)", undeclared.Floor)
	}
}

// The floorplan namespace is named in local config alongside the devices one, and
// is REQUIRED like it. Both are documents that either exist or do not, and an
// instance that names neither cannot say so: it serves ids where names belong and
// reports every storey order as unknown, which is indistinguishable from a
// floorplan that publishes nothing. Naming it is one line; discovering it is
// missing means reading a chart legend and noticing it looks wrong.
func TestLoad_FloorplanNamespace(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "config.yaml", `
site:
  id: "test"
  devices_namespace: "devices_test"
  floorplan_namespace: "floorplan_test"
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Site.FloorplanNamespace != "floorplan_test" {
		t.Errorf("floorplan_namespace = %q, want floorplan_test", cfg.Site.FloorplanNamespace)
	}
	if len(cfg.Warnings()) != 0 {
		t.Errorf("warnings = %v, want none from a fully named site", cfg.Warnings())
	}
}

func TestResolve_RefusesASiteThatNamesNoFloorplanNamespace(t *testing.T) {
	sites := Sites{Sites: []SiteRecord{{ID: "cottage", DevicesNamespace: "devices_cottage"}}}
	_, _, err := ResolveSiteNamespaces(SiteConfig{ID: "cottage"}, sites)
	if err == nil {
		t.Fatal("a site naming no floorplan_namespace must refuse to start")
	}
	if !strings.Contains(err.Error(), "floorplan_namespace") {
		t.Errorf("the error must name the missing key; got %q", err)
	}
	// An operator running two instances needs to know which config to edit without
	// finding the README first.
	if !strings.Contains(err.Error(), "cottage") {
		t.Errorf("the error must name the site it is refusing; got %q", err)
	}
}

// A config missing BOTH namespaces reports the devices one: it is the namespace
// that decides whether any answer is right at all, and an operator fixing one key
// at a time should be sent to that one first.
func TestResolve_MissingBothNamespacesReportsDevicesFirst(t *testing.T) {
	sites := Sites{Sites: []SiteRecord{{ID: "cottage"}}}
	_, _, err := ResolveSiteNamespaces(SiteConfig{ID: "cottage"}, sites)
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if !strings.Contains(err.Error(), "devices_namespace") {
		t.Errorf("want the devices_namespace refusal first; got %q", err)
	}
}

// A hybrid document — wrapper shape, but one collection published as an object —
// used to drop every record in that collection with no error and no warning,
// which is indistinguishable from a floorplan that publishes none. That is the
// exact failure the required-namespace and cold-start refusals exist to remove,
// arriving one layer further in, so it fails loudly instead.
//
// Loud rather than LIBERAL deliberately: greenhouse's decoder has the same
// asymmetry, so accepting an object here would mean the two services read
// different rooms out of one document. Erroring keeps the accepted set identical
// and only changes what happens to a document neither service can read properly.
func TestFloorplanDocument_HybridShapeIsAnError(t *testing.T) {
	for name, raw := range map[string]string{
		"object rooms beside array floors": `{"floors": [{"id": "floor1"}], "rooms": {"floor1.room-a": {"name": "Room A"}}}`,
		"object floors beside array rooms": `{"rooms": [{"id": "floor1.room-a"}], "floors": {"floor1": {"name": "Floor One"}}}`,
		"rooms published as a string":      `{"floors": [{"id": "floor1"}], "rooms": "none"}`,
	} {
		var doc floorplanDocument
		err := json.Unmarshal([]byte(raw), &doc)
		if err == nil {
			t.Errorf("%s: decoded silently as %+v, want an error rather than dropped records", name, doc)
			continue
		}
		if !strings.Contains(err.Error(), "floorplan") {
			t.Errorf("%s: error should name the document: %v", name, err)
		}
	}
}

// A null document is not an empty one. `{}` is a namespace that published no
// records — an answer this service can serve honestly — whereas `null` is a
// publishing mistake that would otherwise latch as a successful fetch and satisfy
// the cold-start check with nothing.
func TestFloorplanDocument_NullIsAnErrorButEmptyIsNot(t *testing.T) {
	var null floorplanDocument
	if err := json.Unmarshal([]byte(`null`), &null); err == nil {
		t.Error("a null floorplan document must not decode as an empty one")
	}

	var empty floorplanDocument
	if err := json.Unmarshal([]byte(`{}`), &empty); err != nil {
		t.Errorf("an empty document is a valid answer (no records published): %v", err)
	}
	if len(empty.Floors) != 0 || len(empty.Rooms) != 0 {
		t.Errorf("empty document = %+v, want no records", empty)
	}
}
