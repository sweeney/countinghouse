package config

import (
	"os"
	"path/filepath"
	"testing"
)

// The devices namespace is named by config rather than hardcoded, so a site can have
// its own. Publishing a per-site namespace does nothing while every service fetches a
// fixed name — which is exactly what happened.
func TestSiteBlockNamesTheDevicesNamespace(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	body := "site:\n  id: home\n  devices_namespace: devices_home\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Site.ID != "home" {
		t.Errorf("Site.ID = %q, want home", cfg.Site.ID)
	}
	if cfg.Site.DevicesNamespace != "devices_home" {
		t.Errorf("Site.DevicesNamespace = %q, want devices_home", cfg.Site.DevicesNamespace)
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
