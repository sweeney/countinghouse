package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// specPaths returns the set of path keys defined in the OpenAPI spec JSON.
func specPaths(t *testing.T, body []byte) map[string]struct{} {
	t.Helper()
	var doc struct {
		Paths map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("unmarshal spec: %v", err)
	}
	out := make(map[string]struct{}, len(doc.Paths))
	for p := range doc.Paths {
		out[p] = struct{}{}
	}
	return out
}

// registeredPaths returns the route patterns newMux actually registers, read from
// the source of server.go.
//
// It used to be a hand-maintained slice whose comment claimed "the path coverage
// test will catch drift" — which it could not, because the test compared that
// slice against the spec and never against the mux. A route added to newMux and to
// neither passed both checks silently, which is how four /prices routes reached a
// green build undocumented.
//
// Reading the source is unusual but it is the only way to get this right: net/http
// offers no way to enumerate a ServeMux's patterns, so the alternative is a second
// hand-maintained list that can drift exactly as the first one did.
func registeredPaths(t *testing.T) []string {
	t.Helper()
	src, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatalf("read server.go to enumerate routes: %v", err)
	}
	// Matches mux.Handle("GET /x", …) and mux.HandleFunc("/x", …).
	re := regexp.MustCompile(`mux\.Handle(?:Func)?\("(?:[A-Z]+ )?([^"]+)"`)
	var out []string
	seen := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(string(src), -1) {
		if p := m[1]; !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		t.Fatal("found no routes in server.go; the pattern this test greps for has changed")
	}
	return out
}

func TestOpenAPIJSON_PublicNoAuth(t *testing.T) {
	s := setup(t)
	s.IdentityURL = "https://id.example.com" // auth configured but must not apply to spec
	mux := newMux(s)

	req := httptest.NewRequest(http.MethodGet, "/openapi.json", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Errorf("want application/json; charset=utf-8, got %q", ct)
	}
}

func TestOpenAPIJSON_ValidJSON(t *testing.T) {
	s := setup(t)
	mux := newMux(s)

	req := httptest.NewRequest(http.MethodGet, "/openapi.json", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	var doc map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
}

func TestOpenAPIJSON_SpecStructure(t *testing.T) {
	s := setup(t)
	mux := newMux(s)

	req := httptest.NewRequest(http.MethodGet, "/openapi.json", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	var doc map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"openapi", "info", "paths", "components"} {
		if _, ok := doc[key]; !ok {
			t.Errorf("spec missing top-level key %q", key)
		}
	}
}

func TestOpenAPIJSON_PublicURLSubstituted(t *testing.T) {
	s := setup(t)
	s.PublicURL = "https://countinghouse.swee.net"
	mux := newMux(s)

	req := httptest.NewRequest(http.MethodGet, "/openapi.json", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	body := w.Body.String()
	if want := "https://countinghouse.swee.net"; !strings.Contains(body, want) {
		t.Errorf("expected substituted public URL %q in spec, got: %s", want, body)
	}
	if strings.Contains(body, "__PUBLIC_URL__") {
		t.Errorf("placeholder __PUBLIC_URL__ was not substituted: %s", body)
	}
}

func TestOpenAPIJSON_PathCoverage(t *testing.T) {
	s := setup(t)
	mux := newMux(s)

	req := httptest.NewRequest(http.MethodGet, "/openapi.json", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	specSet := specPaths(t, w.Body.Bytes())

	routes := registeredPaths(t)
	wantSet := make(map[string]struct{}, len(routes))
	for _, p := range routes {
		wantSet[p] = struct{}{}
	}

	var missing, extra []string
	for p := range wantSet {
		if _, ok := specSet[p]; !ok {
			missing = append(missing, p)
		}
	}
	for p := range specSet {
		if _, ok := wantSet[p]; !ok {
			extra = append(extra, p)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)

	if len(missing) > 0 {
		t.Errorf("paths registered in newMux but missing from openapi.yaml: %v", missing)
	}
	if len(extra) > 0 {
		t.Errorf("paths in openapi.yaml but not registered in newMux: %v", extra)
	}
}
