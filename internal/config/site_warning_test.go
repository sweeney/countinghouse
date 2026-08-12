package config

import (
	"testing"

	"gopkg.in/yaml.v3"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	return writeFile(t, t.TempDir(), "config.yaml", body)
}

// An id with no namespace used to warn and fall back to the shared document. It is now
// a refusal to load, because that document was deleted upstream and the fallback serves
// zero devices while looking healthy. Pinned in namespace_required_test.go.

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

// The deployed shape must stay silent, or the warning is noise and gets ignored —
// which is the failure mode of every warning that cries wolf. "No site block at all"
// is no longer among the silent cases: it names no namespace, so it refuses to load.
func TestCorrectlyConfiguredSitesAreSilent(t *testing.T) {
	cfg, err := Load(writeConfig(t, "site:\n  id: home\n  devices_namespace: devices_home\n"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if w := cfg.Warnings(); len(w) != 0 {
		t.Errorf("want no warnings, got %v", w)
	}
}

// The bare-id form is what statehouse has deployed, so the dual-form decoder must keep
// accepting it — that is a property of UnmarshalYAML and is asserted here directly,
// because the scalar spelling cannot express a namespace and so can never survive Load.
// Back-compat in the parser is not the same as being a usable config: the refusal is
// pinned in namespace_required_test.go.
func TestScalarSiteFormStillParses(t *testing.T) {
	var cfg Config
	if err := yaml.Unmarshal([]byte("site: home\n"), &cfg); err != nil {
		t.Fatalf("the deployed scalar spelling must keep parsing: %v", err)
	}
	if cfg.Site.ID != "home" {
		t.Errorf("Site.ID = %q, want home", cfg.Site.ID)
	}
	if cfg.Site.DevicesNamespace != "" {
		t.Errorf("the scalar form names no namespace, got %q", cfg.Site.DevicesNamespace)
	}
}
