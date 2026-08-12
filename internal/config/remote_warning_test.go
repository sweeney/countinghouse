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

// A misconfigured devices_namespace is the failure this feature newly makes possible,
// and it is close to silent: the fetch 404s, fail-open keeps an empty snapshot,
// /healthz still says ok, and every endpoint truthfully reports zero devices. The one
// log line an operator has to work from must therefore name the namespace actually
// requested — not a hardcoded one that is fine and was never asked for.
func TestFetchFailureNamesTheConfiguredNamespace(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no such namespace", http.StatusNotFound)
	}))
	defer srv.Close()

	var logged bytes.Buffer
	f := &Fetcher{
		BaseURL:          srv.URL,
		Tokens:           &staticTokenSource{"token"},
		Logger:           slog.New(slog.NewTextHandler(&logged, nil)),
		DevicesNamespace: "devices_hme",
	}
	f.Refresh(context.Background())

	out := logged.String()
	if !strings.Contains(out, "devices_hme") {
		t.Errorf("the fetch warning must name the namespace it asked for; got:\n%s", out)
	}
	if strings.Contains(out, "devices_home") {
		t.Errorf("the warning names a namespace that was never requested:\n%s", out)
	}
}

// The error itself is what surfaces on /healthz and in the log line above, so a bare
// "unexpected status 404" leaves the operator with nothing to search for.
func TestFetchErrorIdentifiesTheNamespace(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no such namespace", http.StatusNotFound)
	}))
	defer srv.Close()

	f := &Fetcher{BaseURL: srv.URL, Tokens: &staticTokenSource{"token"}, DevicesNamespace: "devices_home"}
	err := f.fetch(context.Background(), "token", "devices_hme", &map[string]DeviceConfig{})
	if err == nil {
		t.Fatal("want an error for a 404")
	}
	if !strings.Contains(err.Error(), "devices_hme") {
		t.Errorf("fetch error = %q, want it to name the namespace", err)
	}
}
