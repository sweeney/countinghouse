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

// A config with no site block keeps reading the shared namespace it always read, so
// deploying the binary before editing the host's config changes nothing.
func TestDevicesNamespaceDefaultsToTheSharedOne(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte("http:\n  listen: :8080\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Site.DevicesNamespace != DefaultDevicesNamespace {
		t.Errorf("DevicesNamespace = %q, want %q", cfg.Site.DevicesNamespace, DefaultDevicesNamespace)
	}
}

// Naming the namespace in config is only useful if the fetcher reads it. Publishing a
// per-site namespace did nothing while this was a package-level constant.
func TestFetcherReadsTheConfiguredNamespace(t *testing.T) {
	f := &Fetcher{}
	if got := f.devicesNamespace(); got != DefaultDevicesNamespace {
		t.Errorf("unset: %q, want the shared default %q", got, DefaultDevicesNamespace)
	}
	f.DevicesNamespace = "devices_home"
	if got := f.devicesNamespace(); got != "devices_home" {
		t.Errorf("configured: %q, want devices_home", got)
	}
}
