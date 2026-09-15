package config

import (
	"os"
	"path/filepath"
	"testing"
)

// The devices namespace is named by CONFIG rather than hardcoded, so a site can have
// its own. Publishing a per-site namespace does nothing while every service fetches a
// fixed name — which is exactly what happened.
//
// It is now named in the shared `sites` document rather than in each service's local
// config, which is the same lesson one layer further out: `sites` already published
// it per site, and countinghouse redeclaring it locally meant a rename there did
// nothing here.
func TestSitesNamesTheDevicesNamespace(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	// Local config names only which property this instance serves.
	if err := os.WriteFile(p, []byte("site:\n  id: home\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Site.ID != "home" {
		t.Errorf("Site.ID = %q, want home", cfg.Site.ID)
	}

	sites := Sites{Sites: []SiteRecord{{
		ID: "home", DevicesNamespace: "devices_home", FloorplanNamespace: "floorplan_home",
	}}}
	resolved, _, err := ResolveSiteNamespaces(cfg.Site, sites)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resolved.Devices != "devices_home" {
		t.Errorf("resolved devices namespace = %q, want devices_home", resolved.Devices)
	}
}

// Naming the namespace in config is only useful if the fetcher reads it. Publishing a
// per-site namespace did nothing while this was a package-level constant.
//
// The unset case is no longer a default but a refusal, pinned in
// namespace_required_test.go — there is no shared document left to fall back to.
func TestFetcherReadsTheConfiguredNamespace(t *testing.T) {
	f := &Fetcher{DevicesNamespace: "devices_home"}
	if got := f.devicesNamespace(); got != "devices_home" {
		t.Errorf("configured: %q, want devices_home", got)
	}
}
