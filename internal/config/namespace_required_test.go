package config

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// `statehouse_devices` was deleted from the config service on 2026-08-12: fetching it
// now returns 404 not_found. The default therefore named a document that does not
// exist, and the guarantee it existed to provide — "a config predating the per-site
// split keeps reading exactly what it always read" — had nothing left to honour.
//
// What remained was only a way to fail quietly. Every layer is individually correct and
// the combination serves nothing: the fetch 404s, Refresh is fail-open so it keeps the
// last-known snapshot, at startup there is no last-known snapshot, and every endpoint
// then honestly reports zero devices. For countinghouse specifically that means every
// bill and every series answers zero rather than erroring — a wrong number that looks
// like a right one.
//
// So not naming the namespace is now a refusal to start. A host that cannot work
// says so at startup instead of booting empty.
// The refusal MOVED when the pointers became facts in the shared `sites` namespace:
// Load no longer sees them, so it can no longer be the one to refuse. Resolution is,
// and it is strictly the better place — it refuses when NO source supplies the
// namespace, where Load could only ever check one of them.
func TestResolveRefusesASiteThatNamesNoDevicesNamespace(t *testing.T) {
	sites := Sites{Sites: []SiteRecord{{ID: "cottage", FloorplanNamespace: "floorplan_cottage"}}}
	_, _, err := ResolveSiteNamespaces(SiteConfig{ID: "cottage"}, sites)
	if err == nil {
		t.Fatal("a site naming no devices_namespace must refuse to start, not warn and continue")
	}
	if !strings.Contains(err.Error(), "devices_namespace") {
		t.Errorf("the error must name the missing key; got %q", err)
	}
	if !strings.Contains(err.Error(), "cottage") {
		t.Errorf("the error must name the site it is refusing, so an operator running two\n"+
			"instances knows which config to edit; got %q", err)
	}
}

// devices_namespace has NO local fallback, unlike floorplan and agreements. A stale
// local copy of it would not degrade a label — it would bill ANOTHER PROPERTY'S
// devices while the service looked entirely healthy. So a local value must not
// rescue a site entry that omits it.
func TestResolveHasNoLocalFallbackForDevices(t *testing.T) {
	sites := Sites{Sites: []SiteRecord{{ID: "home", FloorplanNamespace: "floorplan_home"}}}
	local := SiteConfig{ID: "home", FloorplanNamespace: "floorplan_home"}

	if _, _, err := ResolveSiteNamespaces(local, sites); err == nil {
		t.Fatal("resolution must refuse; there is no local devices_namespace to fall back to")
	}
	// And it says so, rather than just reporting the value missing.
	_, _, err := ResolveSiteNamespaces(local, sites)
	if !strings.Contains(err.Error(), "sites") {
		t.Errorf("the error should point at the sites namespace as the place to fix it; got %q", err)
	}
}

// A config with no site block at all is the same failure wearing a different hat. It
// now fails one step earlier and for a sharper reason: with no site id there is
// nothing to look up in `sites`, so not one pointer can be resolved.
func TestResolveRefusesAConfigWithNoSiteBlock(t *testing.T) {
	cfg, err := Load(writeConfig(t, "http:\n  listen: \":8585\"\n"))
	if err != nil {
		t.Fatalf("Load itself no longer refuses this; resolution does: %v", err)
	}
	_, _, err = ResolveSiteNamespaces(cfg.Site, Sites{Sites: []SiteRecord{{ID: "home"}}})
	if err == nil {
		t.Fatal("a config with no site block cannot resolve anything, so it must refuse")
	}
	if !strings.Contains(err.Error(), "site.id") {
		t.Errorf("the error must name the missing key; got %q", err)
	}
}

// The scalar form's status has INVERTED, which is worth pinning rather than quietly
// dropping. It names an id and no namespaces — once the shape this refusal existed to
// catch, and now exactly the right config, because the namespaces belong in `sites`
// and the id is the only thing an instance must declare for itself.
func TestScalarSiteFormIsNowSufficient(t *testing.T) {
	cfg, err := Load(writeConfig(t, "site: home\n"))
	if err != nil {
		t.Fatalf("the scalar form must load: %v", err)
	}
	if cfg.Site.ID != "home" {
		t.Fatalf("site id = %q, want home", cfg.Site.ID)
	}
	sites := Sites{Sites: []SiteRecord{{
		ID: "home", DevicesNamespace: "devices_home", FloorplanNamespace: "floorplan_home",
	}}}
	got, warns, err := ResolveSiteNamespaces(cfg.Site, sites)
	if err != nil {
		t.Fatalf("an id plus a sites entry is a complete configuration: %v", err)
	}
	if len(warns) != 0 {
		t.Errorf("nothing to warn about: %v", warns)
	}
	if got.Devices != "devices_home" || got.Floorplan != "floorplan_home" {
		t.Errorf("resolved = %+v", got)
	}
}

// The error is only useful if it says what to write. An operator reading it at 3am
// should not have to find the README.
func TestTheRefusalSaysWhereToFixIt(t *testing.T) {
	sites := Sites{Sites: []SiteRecord{{ID: "cottage", FloorplanNamespace: "f"}}}
	_, _, err := ResolveSiteNamespaces(SiteConfig{ID: "cottage"}, sites)
	if err == nil {
		t.Fatal("expected a refusal")
	}
	msg := err.Error()
	for _, want := range []string{"devices_namespace", "sites", "cottage"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal must say what to add and where, missing %q in:\n%s", want, msg)
		}
	}
}

// The deployed shape must keep working, or this is an outage rather than a guard.
func TestLoadAcceptsAFullyNamedSite(t *testing.T) {
	cfg, err := Load(writeConfig(t, "site:\n  id: home\n  floorplan_namespace: floorplan_home\n"))
	if err != nil {
		t.Fatalf("the deployed config must keep loading: %v", err)
	}
	if cfg.Site.ID != "home" {
		t.Errorf("Site.ID = %q, want home", cfg.Site.ID)
	}
	if w := cfg.Warnings(); len(w) != 0 {
		t.Errorf("a fully named site must be silent, got %v", w)
	}
}

// The mirror case is deliberately NOT promoted. A namespace with no id fetches the
// right devices and serves correct numbers; it only costs observability, because
// /healthz cannot say which property it serves. Refusing to start over that would take
// down a working instance to fix a label.
func TestNamespaceWithoutASiteIDRemainsOnlyAWarning(t *testing.T) {
	cfg, err := Load(writeConfig(t, "site:\n  floorplan_namespace: floorplan_home\n"))
	if err != nil {
		t.Fatalf("a working-but-unlabelled instance must still start: %v", err)
	}
	if len(cfg.Warnings()) == 0 {
		t.Error("a namespace pointer with no site id must still warn")
	}
}

// The same value was defaulted in two places — Load and the Fetcher — which is the
// shape of bug that recurred throughout this migration. Removing one and leaving the
// other would restore the deleted namespace by the back door for any Fetcher built
// without going through Load.
func TestFetcherHasNoFallbackToTheDeletedNamespace(t *testing.T) {
	f := &Fetcher{}
	if got := f.devicesNamespace(); got != "" {
		t.Errorf("an unset Fetcher must not substitute a namespace of its own, got %q", got)
	}
	f.DevicesNamespace = "devices_home"
	if got := f.devicesNamespace(); got != "devices_home" {
		t.Errorf("configured: %q, want devices_home", got)
	}
}

// Removing the fallback leaves DevicesNamespace empty, and the request path is built by
// concatenation — so an unnamed namespace would have asked for /api/v1/config/. That is
// a different endpoint failing for a reason that says nothing about the real mistake,
// which swaps a silent failure for a confusing one. It must issue no request at all.
func TestFetcherIssuesNoRequestWhenTheNamespaceIsUnnamed(t *testing.T) {
	var asked []string
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{}"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	var logged bytes.Buffer
	f := &Fetcher{
		BaseURL:    srv.URL,
		Tokens:     &staticTokenSource{token: "test-token"},
		HTTPClient: srv.Client(),
		Logger:     slog.New(slog.NewTextHandler(&logged, nil)),
		// DevicesNamespace deliberately unset.
	}
	f.Refresh(context.Background())

	for _, p := range asked {
		if strings.HasSuffix(p, "/api/v1/config/") {
			t.Errorf("asked for the empty namespace %q; it must skip the fetch entirely", p)
		}
	}
	if !strings.Contains(logged.String(), "no devices namespace") {
		t.Errorf("skipping must say why; got:\n%s", logged.String())
	}
	if _, ok := f.Statuses()[""]; ok {
		t.Error("an empty namespace must not be recorded as a status key")
	}
}

// Nothing anywhere may still spell the deleted namespace, in a default or a message.
func TestTheDeletedNamespaceIsNotReferencedAsADefault(t *testing.T) {
	sites := Sites{Sites: []SiteRecord{{
		ID: "home", DevicesNamespace: "devices_home", FloorplanNamespace: "floorplan_home",
	}}}
	resolved, _, err := ResolveSiteNamespaces(SiteConfig{ID: "home"}, sites)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Devices == "statehouse_devices" {
		t.Error("statehouse_devices is deleted upstream; it must not be reachable as a value")
	}
	f := &Fetcher{DevicesNamespace: resolved.Devices}
	if f.devicesNamespace() == "statehouse_devices" {
		t.Error("the fetcher must not resolve to the deleted namespace")
	}
}
