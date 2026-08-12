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
func TestLoadRefusesASiteThatNamesNoDevicesNamespace(t *testing.T) {
	_, err := Load(writeConfig(t, "site:\n  id: cottage\n"))
	if err == nil {
		t.Fatal("a site naming no devices_namespace must refuse to load, not warn and continue")
	}
	if !strings.Contains(err.Error(), "devices_namespace") {
		t.Errorf("the error must name the missing key; got %q", err)
	}
	if !strings.Contains(err.Error(), "cottage") {
		t.Errorf("the error must name the site it is refusing, so an operator running two\n"+
			"instances knows which config to edit; got %q", err)
	}
}

// A config with no site block at all is the same failure wearing a different hat: it
// names no namespace either, and previously took the deleted default.
func TestLoadRefusesAConfigWithNoSiteBlock(t *testing.T) {
	_, err := Load(writeConfig(t, "http:\n  listen: \":8585\"\n"))
	if err == nil {
		t.Fatal("a config with no site block names no devices namespace, so it must refuse")
	}
	if !strings.Contains(err.Error(), "devices_namespace") {
		t.Errorf("the error must name the missing key; got %q", err)
	}
}

// The scalar form still parses — it is what statehouse has deployed — but parsing is
// not the same as being usable. It names an id and no namespace, which is precisely
// the shape this refusal exists to catch.
func TestLoadRefusesTheScalarSiteForm(t *testing.T) {
	_, err := Load(writeConfig(t, "site: home\n"))
	if err == nil {
		t.Fatal("the scalar form names no namespace, so it must refuse like any other id-only site")
	}
}

// The error is only useful if it says what to write. An operator reading it at 3am
// should not have to find the README.
func TestTheRefusalShowsTheBlockToAdd(t *testing.T) {
	_, err := Load(writeConfig(t, "site:\n  id: cottage\n"))
	if err == nil {
		t.Fatal("expected a refusal")
	}
	msg := err.Error()
	for _, want := range []string{"site:", "id:", "devices_namespace:"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal must show the block to add, missing %q in:\n%s", want, msg)
		}
	}
}

// The deployed shape must keep working, or this is an outage rather than a guard.
func TestLoadAcceptsAFullyNamedSite(t *testing.T) {
	cfg, err := Load(writeConfig(t, "site:\n  id: home\n  devices_namespace: devices_home\n"))
	if err != nil {
		t.Fatalf("the deployed config must keep loading: %v", err)
	}
	if cfg.Site.DevicesNamespace != "devices_home" {
		t.Errorf("DevicesNamespace = %q, want devices_home", cfg.Site.DevicesNamespace)
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
	cfg, err := Load(writeConfig(t, "site:\n  devices_namespace: devices_home\n"))
	if err != nil {
		t.Fatalf("a working-but-unlabelled instance must still start: %v", err)
	}
	if len(cfg.Warnings()) == 0 {
		t.Error("a devices_namespace with no site id must still warn")
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
	cfg, err := Load(writeConfig(t, "site:\n  id: home\n  devices_namespace: devices_home\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Site.DevicesNamespace == "statehouse_devices" {
		t.Error("statehouse_devices is deleted upstream; it must not be reachable as a value")
	}
	f := &Fetcher{DevicesNamespace: cfg.Site.DevicesNamespace}
	if f.devicesNamespace() == "statehouse_devices" {
		t.Error("the fetcher must not resolve to the deleted namespace")
	}
}
