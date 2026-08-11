package config

import (
	"strings"
	"testing"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	return writeFile(t, t.TempDir(), "config.yaml", body)
}

// The site block has two independently-optional keys, and getting the pair wrong is
// the one misconfiguration the per-site split newly makes possible. A host configured
// for another property but not told where that property's devices live falls back to
// the shared namespace and charts this house's consumption as the cottage's: no error,
// no empty response, just a plausible-looking bill for the wrong building.
//
// Nothing else catches it. The fetch succeeds, /healthz is green, and every endpoint
// answers. So Load records it and main logs it at startup.
func TestSiteIDWithoutANamespaceIsWarnedAbout(t *testing.T) {
	cfg, err := Load(writeConfig(t, "site:\n  id: cottage\n"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	warnings := strings.Join(cfg.Warnings(), "\n")
	if !strings.Contains(warnings, "cottage") || !strings.Contains(warnings, DefaultDevicesNamespace) {
		t.Errorf("a site id with no devices_namespace must warn, naming both the site and\n"+
			"the namespace it fell back to; got %q", warnings)
	}
}

// The mirror case: a namespace with no site to own it. Harmless today — the fetch
// works and the right devices load — but /healthz then reports an instance that
// cannot say which property it serves, which is the question the block exists to
// answer.
func TestNamespaceWithoutASiteIDIsWarnedAbout(t *testing.T) {
	cfg, err := Load(writeConfig(t, "site:\n  devices_namespace: devices_home\n"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(cfg.Warnings()) == 0 {
		t.Error("a devices_namespace with no site id must warn")
	}
}

// Both spellings that are actually deployed must stay silent, or the warning is noise
// and gets ignored — which is the failure mode of every warning that cries wolf.
func TestCorrectlyConfiguredSitesAreSilent(t *testing.T) {
	for _, tc := range []struct {
		name string
		yaml string
	}{
		{"both keys given", "site:\n  id: home\n  devices_namespace: devices_home\n"},
		{"no site block at all", "http:\n  listen: \":8585\"\n"},
	} {
		cfg, err := Load(writeConfig(t, tc.yaml))
		if err != nil {
			t.Fatalf("%s: load: %v", tc.name, err)
		}
		if w := cfg.Warnings(); len(w) != 0 {
			t.Errorf("%s: want no warnings, got %v", tc.name, w)
		}
	}
}

// The bare-id form is what statehouse has deployed, so it must parse — but it is also
// exactly the shape that leaves the namespace unnamed. It parses AND warns: back-compat
// is not the same as being correct for a second site.
func TestScalarSiteFormParsesAndWarns(t *testing.T) {
	cfg, err := Load(writeConfig(t, "site: home\n"))
	if err != nil {
		t.Fatalf("the deployed scalar spelling must keep parsing: %v", err)
	}
	if cfg.Site.ID != "home" {
		t.Errorf("Site.ID = %q, want home", cfg.Site.ID)
	}
	if cfg.Site.DevicesNamespace != DefaultDevicesNamespace {
		t.Errorf("DevicesNamespace = %q, want the pre-migration default", cfg.Site.DevicesNamespace)
	}
	if len(cfg.Warnings()) == 0 {
		t.Error("the scalar form names no namespace, so it must warn like any other id-only site")
	}
}
