package config

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// The `sites` namespace as the source of per-site namespace pointers.
//
// Before this, countinghouse redeclared devices_namespace and
// floorplan_namespace in its own local config while `sites` already held them —
// and greenhouse read them from there. That duplication drifts in one direction
// and fails silently: rename a namespace in `sites`, greenhouse follows, and
// countinghouse keeps reading the old one with nothing to indicate it.
//
// The split that remains is deliberate. `site.id` stays LOCAL, because "which
// property am I?" is a deployment decision like the listen port. The namespace
// pointers are facts ABOUT the property, so they come from `sites`.
// ---------------------------------------------------------------------------

// The shape as it should be authored, using the renamed key. Not a verbatim copy
// of the live document, which is free to carry extra fields we ignore.
//
// Every LOCATION DETAIL here is a placeholder: coordinates, scheme names, and the
// second site's id and name. This repo is public, and a property is identifiable by
// any of them — a latitude to four decimal places is a street address, and a named
// building is worse. The coordinates are round numbers that exercise the same
// parsing (positive and negative, integral and fractional), and the second site
// exists only to prove that a record with no namespace pointers resolves and that
// Find picks the right one of several. Nothing in countinghouse reads the
// coordinates at all; they are here to prove unknown fields are tolerated rather
// than rejected.
const sitesDocFixture = `{
  "sites": [
    {
      "id": "home",
      "name": "Home",
      "latitude": 51.5,
      "longitude": -0.1,
      "floorplan_namespace": "floorplan_home",
      "devices_namespace": "devices_home",
      "energy_agreements_namespace": "energy_agreements",
      "bin_scheme": "scheme_one"
    },
    {
      "id": "cottage",
      "name": "Second Site",
      "latitude": 52.0,
      "longitude": 1.0,
      "bin_scheme": "scheme_two"
    }
  ]
}`

func parseSites(t *testing.T, doc string) Sites {
	t.Helper()
	var s Sites
	if err := json.Unmarshal([]byte(doc), &s); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return s
}

// The document carries fields countinghouse does not use (coordinates, MQTT
// topics, bin scheme). Those must be ignored rather than rejected: `sites` is
// shared with other services and will grow keys that are none of our business.
func TestSitesParsesSharedDocumentAndIgnoresForeignFields(t *testing.T) {
	s := parseSites(t, sitesDocFixture)
	if len(s.Sites) != 2 {
		t.Fatalf("got %d sites, want 2", len(s.Sites))
	}

	home, ok := s.Find("home")
	if !ok {
		t.Fatal("home not found")
	}
	if home.DevicesNamespace != "devices_home" ||
		home.FloorplanNamespace != "floorplan_home" ||
		home.EnergyAgreementsNamespace != "energy_agreements" {
		t.Errorf("home pointers = %+v", home)
	}
	if home.Name != "Home" {
		t.Errorf("name = %q", home.Name)
	}
}

func TestSitesFind(t *testing.T) {
	s := parseSites(t, sitesDocFixture)

	if _, ok := s.Find("nonexistent"); ok {
		t.Error("an unknown site id must not resolve")
	}
	// Case matters: ids are keys, not labels.
	if _, ok := s.Find("Home"); ok {
		t.Error("site lookup should be case-sensitive")
	}
	// A site present but with no pointers resolves — it exists — and the caller
	// decides whether the missing pointers are fatal.
	second, ok := s.Find("cottage")
	if !ok {
		t.Fatal("cottage should resolve; it IS in the document")
	}
	if second.DevicesNamespace != "" || second.EnergyAgreementsNamespace != "" {
		t.Errorf("cottage should have no pointers configured: %+v", second)
	}
}

// ---------------------------------------------------------------------------
// Resolution against local config
// ---------------------------------------------------------------------------

func TestResolveSiteNamespaces(t *testing.T) {
	sites := parseSites(t, sitesDocFixture)

	for _, tc := range []struct {
		name        string
		local       SiteConfig
		sites       Sites
		wantDevices string
		wantFloor   string
		wantAgree   string
		wantErr     string
		wantWarn    string
	}{
		{
			name:        "everything comes from sites when local says only the id",
			local:       SiteConfig{ID: "home"},
			sites:       sites,
			wantDevices: "devices_home", wantFloor: "floorplan_home",
			wantAgree: "energy_agreements",
		},
		{
			// sites WINS over a disagreeing local value, because a stale local value
			// silently overriding the correct remote one is the drift this exists to
			// fix. But the disagreement is reported rather than swallowed — an
			// operator's edit must never be quietly ignored.
			name:        "sites wins over a disagreeing local fallback, loudly",
			local:       SiteConfig{ID: "home", FloorplanNamespace: "floorplan_old"},
			sites:       sites,
			wantDevices: "devices_home", wantFloor: "floorplan_home",
			wantAgree: "energy_agreements",
			wantWarn:  "floorplan_old",
		},
		{
			name:        "a local value agreeing with sites warns about nothing",
			local:       SiteConfig{ID: "home", FloorplanNamespace: "floorplan_home"},
			sites:       sites,
			wantDevices: "devices_home", wantFloor: "floorplan_home",
			wantAgree: "energy_agreements",
		},
		{
			// Local config remains the fallback for floorplan and agreements, which
			// is what makes the migration safe for a partly-filled site entry.
			name: "local fills in the floorplan that sites omits",
			local: SiteConfig{
				ID: "cottage", FloorplanNamespace: "floorplan_cottage",
			},
			sites: Sites{Sites: []SiteRecord{
				{ID: "cottage", DevicesNamespace: "devices_cottage"},
			}},
			wantDevices: "devices_cottage", wantFloor: "floorplan_cottage",
		},
		{
			// devices_namespace has NO local fallback. It decides whether any answer
			// is right at all, so a stale local copy would bill another property's
			// devices while the service looked healthy.
			name:    "a devices namespace missing from sites is refused, with no local rescue",
			local:   SiteConfig{ID: "cottage", FloorplanNamespace: "f"},
			sites:   Sites{Sites: []SiteRecord{{ID: "cottage"}}},
			wantErr: "devices_namespace",
		},
		{
			// An empty agreements pointer is NOT an error: it means stay on the
			// legacy tariff document, which is the opt-in migration path.
			name:  "no agreements pointer anywhere is legal",
			local: SiteConfig{ID: "cottage", FloorplanNamespace: "f"},
			sites: Sites{Sites: []SiteRecord{
				{ID: "cottage", DevicesNamespace: "d"},
			}},
			wantDevices: "d", wantFloor: "f", wantAgree: "",
		},
		{
			// Pointing at a site that is not in the document is a configuration
			// error, not something to paper over: we would be serving a property
			// nobody has described.
			name:    "an unknown site id is refused",
			local:   SiteConfig{ID: "nowhere", FloorplanNamespace: "f"},
			sites:   sites,
			wantErr: "nowhere",
		},
		{
			// The existing rule, unchanged: an unnamed floorplan degrades to
			// ids-as-labels, which is silence that reads as data.
			name:  "a missing floorplan namespace is refused",
			local: SiteConfig{ID: "cottage"},
			sites: Sites{Sites: []SiteRecord{
				{ID: "cottage", DevicesNamespace: "d"},
			}},
			wantErr: "floorplan_namespace",
		},
		{
			// With no sites document at all — a local-dev instance with no remote
			// config — resolution still refuses, because devices has no local
			// source. That is correct: such an instance fetches nothing anyway and
			// main.go never calls this.
			name:    "no sites document means no devices namespace",
			local:   SiteConfig{ID: "home", FloorplanNamespace: "f"},
			sites:   Sites{},
			wantErr: "devices_namespace",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, warns, err := ResolveSiteNamespaces(tc.local, tc.sites)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("want an error mentioning %q, got %+v", tc.wantErr, got)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("error %q should mention %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Devices != tc.wantDevices {
				t.Errorf("devices = %q, want %q", got.Devices, tc.wantDevices)
			}
			if got.Floorplan != tc.wantFloor {
				t.Errorf("floorplan = %q, want %q", got.Floorplan, tc.wantFloor)
			}
			if got.Agreements != tc.wantAgree {
				t.Errorf("agreements = %q, want %q", got.Agreements, tc.wantAgree)
			}
			joined := strings.Join(warns, " | ")
			if tc.wantWarn == "" {
				if len(warns) != 0 {
					t.Errorf("unexpected warnings: %v", warns)
				}
			} else if !strings.Contains(joined, tc.wantWarn) {
				t.Errorf("warnings %q should mention the overridden value %q", joined, tc.wantWarn)
			}
		})
	}
}

// An id is required: an instance that does not know which property it serves
// cannot resolve anything, and guessing "the only site" would break the moment a
// second one appeared.
func TestResolveSiteNamespacesRequiresAnID(t *testing.T) {
	_, _, err := ResolveSiteNamespaces(SiteConfig{FloorplanNamespace: "f"},
		parseSites(t, sitesDocFixture))
	if err == nil {
		t.Fatal("want an error when site.id is empty")
	}
	if !strings.Contains(err.Error(), "site.id") {
		t.Errorf("error %q should name site.id", err)
	}
}

// The pointers are resolved ONCE, at startup. A SIGHUP that finds them changed
// must say so rather than silently repointing a running service at a different
// property's data — adopting new topology mid-flight is not something to do
// quietly, and a restart is cheap and explicit.
func TestDriftedNamespacesAreReported(t *testing.T) {
	resolved := SiteNamespaces{Devices: "devices_home", Floorplan: "floorplan_home", Agreements: "energy_agreements"}

	t.Run("no drift is silent", func(t *testing.T) {
		if got := resolved.DriftFrom(parseSites(t, sitesDocFixture), "home"); len(got) != 0 {
			t.Errorf("unexpected drift: %v", got)
		}
	})

	t.Run("a changed pointer is reported", func(t *testing.T) {
		moved := strings.Replace(sitesDocFixture, `"devices_home"`, `"devices_home_v2"`, 1)
		got := resolved.DriftFrom(parseSites(t, moved), "home")
		if len(got) == 0 {
			t.Fatal("a changed devices namespace should be reported")
		}
		joined := strings.Join(got, " | ")
		for _, want := range []string{"devices_home", "devices_home_v2", "restart"} {
			if !strings.Contains(joined, want) {
				t.Errorf("drift report %q should mention %q", joined, want)
			}
		}
	})

	t.Run("a site that vanished is reported", func(t *testing.T) {
		if got := resolved.DriftFrom(Sites{}, "home"); len(got) == 0 {
			t.Error("a site missing from the document should be reported")
		}
	})
}

// ---------------------------------------------------------------------------
// Two-phase bootstrap
// ---------------------------------------------------------------------------

// The Fetcher must read `sites` BEFORE it can know which other namespaces to
// read. That makes `sites` boot-critical: without it we cannot even name what we
// need, so there is nothing to fail open onto.
func TestFetcherResolvesNamespacesFromSites(t *testing.T) {
	mux := http.NewServeMux()
	serveNamespace(mux, nsSites, json.RawMessage(sitesDocFixture))
	f := newTestFetcher(t, mux, &staticTokenSource{token: "test-token"})
	// Local config names only the site; everything else comes from `sites`.
	f.DevicesNamespace, f.FloorplanNamespace, f.EnergyAgreementsNamespace = "", "", ""

	warns, err := f.ResolveNamespaces(context.Background(), SiteConfig{ID: "home"})
	if err != nil {
		t.Fatalf("ResolveNamespaces: %v", err)
	}
	if len(warns) != 0 {
		t.Errorf("unexpected warnings: %v", warns)
	}
	if f.DevicesNamespace != "devices_home" {
		t.Errorf("devices namespace = %q", f.DevicesNamespace)
	}
	if f.FloorplanNamespace != "floorplan_home" {
		t.Errorf("floorplan namespace = %q", f.FloorplanNamespace)
	}
	if f.EnergyAgreementsNamespace != "energy_agreements" {
		t.Errorf("agreements namespace = %q", f.EnergyAgreementsNamespace)
	}

	// Phase one has fetched ONLY sites. The namespaces it just named are still
	// cold, and must be — the cold-start check is what forces phase two to happen
	// before the service answers anything.
	if st, ok := f.Statuses()[nsSites]; !ok || !st.OK {
		t.Errorf("sites status = %+v, want a successful fetch recorded", st)
	}
	cold := map[string]bool{}
	for _, ns := range f.Cold() {
		cold[ns] = true
	}
	if cold[nsSites] {
		t.Error("sites has landed and must not be reported cold")
	}
	for _, ns := range []string{"devices_home", "floorplan_home", "energy_agreements"} {
		if !cold[ns] {
			t.Errorf("%s should still be cold after phase one; only sites has been fetched", ns)
		}
	}

	// Phase two fills them, and then nothing is cold.
	serveNamespace(mux, "devices_home", map[string]DeviceConfig{})
	serveNamespace(mux, "floorplan_home", map[string]any{"floors": []any{}, "rooms": []any{}})
	serveNamespace(mux, "energy_agreements", EnergyAgreements{Agreements: map[string][]Agreement{}})
	f.Refresh(context.Background())
	if remaining := f.Cold(); len(remaining) != 0 {
		t.Errorf("Cold() = %v after Refresh, want none", remaining)
	}
}

// An unreachable `sites` at startup cannot be failed open: there is no
// last-known set of pointers to fall back to, and guessing would mean reading
// some other property's data or none at all.
func TestFetcherResolveNamespacesFailsWhenSitesUnavailable(t *testing.T) {
	mux := http.NewServeMux() // serves nothing
	f := newTestFetcher(t, mux, &staticTokenSource{token: "test-token"})
	f.DevicesNamespace, f.FloorplanNamespace = "", ""

	if _, err := f.ResolveNamespaces(context.Background(), SiteConfig{ID: "home"}); err == nil {
		t.Fatal("want an error when the sites namespace cannot be fetched")
	}
	if cold := f.Cold(); len(cold) == 0 {
		t.Error("sites should be reported cold after a failed resolve")
	}
}

// Local config still works alone, which is what keeps local development and an
// un-migrated deployment going.
func TestFetcherResolveNamespacesFallsBackToLocal(t *testing.T) {
	mux := http.NewServeMux()
	// A sites document that knows the site but names none of its namespaces.
	serveNamespace(mux, nsSites, Sites{Sites: []SiteRecord{
		{ID: "home", Name: "Home", DevicesNamespace: "devices_home"},
	}})
	f := newTestFetcher(t, mux, &staticTokenSource{token: "test-token"})

	local := SiteConfig{ID: "home", FloorplanNamespace: "floorplan_local"}
	if _, err := f.ResolveNamespaces(context.Background(), local); err != nil {
		t.Fatalf("ResolveNamespaces: %v", err)
	}
	// Devices still comes from sites (it has no local fallback); floorplan falls back.
	if f.DevicesNamespace != "devices_home" {
		t.Errorf("devices = %q, want the value from sites", f.DevicesNamespace)
	}
	if f.FloorplanNamespace != "floorplan_local" {
		t.Errorf("floorplan = %q, want the local fallback", f.FloorplanNamespace)
	}
}

// Load must NOT require the namespace pointers locally when there is a config
// service to resolve them from — requiring both would force every operator to
// duplicate what `sites` already says, which is the drift this removes. The
// requirement moves to ResolveSiteNamespaces, covered above.
func TestLoadDefersNamespaceRequirementToResolution(t *testing.T) {
	dir := t.TempDir()

	write := func(body string) string {
		t.Helper()
		path := filepath.Join(dir, "c.yaml")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}

	t.Run("with a config service, site.id alone loads", func(t *testing.T) {
		cfg, err := Load(write(`
site: { id: home }
remote_config: { base_url: "https://config.example" }
`))
		if err != nil {
			t.Fatalf("Load should defer to resolution: %v", err)
		}
		if cfg.Site.ID != "home" {
			t.Errorf("site id = %q, want home", cfg.Site.ID)
		}
	})

	t.Run("Load never refuses over the namespace pointers", func(t *testing.T) {
		if _, err := Load(write(`
site: { id: home }
remote_config: { base_url: "https://config.example" }
`)); err != nil {
			t.Fatalf("Load: %v", err)
		}
	})

	t.Run("without a config service, a bare id loads too", func(t *testing.T) {
		// Nothing is fetched in that mode, so neither pointer is ever read and
		// requiring them would refuse a config over values it then ignores.
		if _, err := Load(write(`
site: { id: home }
remote_config: { base_url: "" }
`)); err != nil {
			t.Fatalf("Load: %v", err)
		}
	})
}
