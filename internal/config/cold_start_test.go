package config

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"testing"
)

// coldFetcher wires a Fetcher for all three namespaces against a mux whose
// handlers are toggled per-test.
func coldFetcher(t *testing.T, mux *http.ServeMux) *Fetcher {
	t.Helper()
	f := newTestFetcher(t, mux, &staticTokenSource{token: "test-token"})
	f.FloorplanNamespace = "floorplan_home"
	return f
}

// serveToggle serves a namespace whose availability is flipped through ok.
func serveToggle(mux *http.ServeMux, ns string, ok *bool, body any) {
	mux.HandleFunc("/api/v1/config/"+ns, func(w http.ResponseWriter, _ *http.Request) {
		if !*ok {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	})
}

// A namespace that has never been fetched successfully is COLD: there is no
// last-known snapshot to fall back to, so fail-open falls open onto nothing and
// the service would serve emptiness as though it were an answer.
func TestFetcher_ColdListsNamespacesThatNeverLanded(t *testing.T) {
	mux := http.NewServeMux()
	devicesOK, tariffsOK, floorplanOK := false, true, true
	serveToggle(mux, "devices_home", &devicesOK, map[string]any{})
	serveToggle(mux, "energy_tariffs", &tariffsOK, map[string]any{})
	serveToggle(mux, "floorplan_home", &floorplanOK, map[string]any{})

	f := coldFetcher(t, mux)
	f.Refresh(context.Background())

	if got, want := f.Cold(), []string{"devices_home"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Cold() = %v, want %v", got, want)
	}
}

// Nothing cold once every namespace has landed — the state a healthy boot
// reaches, and the only state in which serving is honest.
func TestFetcher_ColdIsEmptyOnceEverythingLands(t *testing.T) {
	mux := http.NewServeMux()
	ok := true
	serveToggle(mux, "devices_home", &ok, map[string]any{})
	serveToggle(mux, "energy_tariffs", &ok, map[string]any{})
	serveToggle(mux, "floorplan_home", &ok, map[string]any{})

	f := coldFetcher(t, mux)
	f.Refresh(context.Background())

	if got := f.Cold(); len(got) != 0 {
		t.Fatalf("Cold() = %v, want nothing after a clean refresh", got)
	}
}

// The distinction the whole design rests on: STALE is not COLD. A namespace that
// landed once and now fails is serving last-known records — correct, and the
// value fail-open exists to protect. Only /healthz degrades; the process keeps
// running, and a later SIGHUP must never be able to turn that into a refusal.
func TestFetcher_StaleIsNotCold(t *testing.T) {
	mux := http.NewServeMux()
	ok := true
	serveToggle(mux, "devices_home", &ok, map[string]any{})
	serveToggle(mux, "energy_tariffs", &ok, map[string]any{})
	serveToggle(mux, "floorplan_home", &ok, map[string]any{
		"floors": []any{map[string]any{"id": "floor1", "name": "Floor One"}},
	})

	f := coldFetcher(t, mux)
	f.Refresh(context.Background())
	ok = false
	f.Refresh(context.Background())

	if got := f.Cold(); len(got) != 0 {
		t.Fatalf("Cold() = %v, want nothing: these namespaces landed once and are merely stale", got)
	}
	if st := f.Statuses()["floorplan_home"]; st.OK {
		t.Error("a failing refresh must still record the failure for /healthz")
	}
	if name := f.Floors()["floor1"].Name; name != "Floor One" {
		t.Errorf("last-known records lost on a failed refresh: %q", name)
	}
}

// An empty namespace document is an ANSWER, not an absence: the config service
// said "no devices", which is a fact this service can serve honestly. Only the
// absence of any answer is cold.
func TestFetcher_AnEmptyDocumentIsNotCold(t *testing.T) {
	mux := http.NewServeMux()
	ok := true
	serveToggle(mux, "devices_home", &ok, map[string]any{})
	serveToggle(mux, "energy_tariffs", &ok, map[string]any{})
	serveToggle(mux, "floorplan_home", &ok, map[string]any{})

	f := coldFetcher(t, mux)
	f.Refresh(context.Background())

	if got := f.Cold(); len(got) != 0 {
		t.Fatalf("Cold() = %v, want nothing: an empty document was fetched successfully", got)
	}
}

// A Fetcher with no remote config at all attempts nothing, so nothing can have
// landed. It reports every configured namespace as cold and lets the caller
// decide: main.go treats an empty base_url as explicit local-dev opt-out, and
// only enforces the cold check when a config service is actually named.
func TestFetcher_ColdWithNoRemoteConfig(t *testing.T) {
	f := &Fetcher{DevicesNamespace: "devices_home", FloorplanNamespace: "floorplan_home"}
	got := f.Cold()
	want := []string{"devices_home", "energy_tariffs", "floorplan_home"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Cold() = %v, want %v", got, want)
	}
}
