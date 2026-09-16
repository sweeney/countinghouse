package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

// registeredPaths is every path the server actually serves, read from the DECLARED
// route table.
//
// Two earlier versions of this got it wrong in the same way. The first compared a
// hand-maintained slice against the spec and never against the mux, which is how four
// /prices routes reached a green build undocumented. The second parsed server.go with a
// regex, which could only see routes registered in THAT file — so a route in
// prices_handlers.go was invisible and the comparison passed while drifting.
//
// Reading the table removes the class: newMux registers from it, this reads from it, and
// a route absent from it does not exist at runtime either. There is no longer a second
// source that can disagree.
func registeredPaths(t *testing.T) []string {
	t.Helper()
	var out []string
	seen := map[string]bool{}
	for _, rt := range append(append([]route{}, publicRoutes...), dataRoutes...) {
		// Patterns are "GET /x" for data routes and "/x" for public ones; the spec keys
		// on the path alone.
		p := rt.pattern
		if i := strings.IndexByte(p, ' '); i >= 0 {
			p = p[i+1:]
		}
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		t.Fatal("the route table is empty; newMux would serve nothing")
	}
	return out
}

// Every declared route must be REACHABLE, not merely declared. A table entry with a nil
// handler, or a pattern net/http rejects, would otherwise be caught only at runtime — and
// the table is now the single source the spec test trusts.
func TestEveryDeclaredRouteIsReachable(t *testing.T) {
	s := setup(t)
	mux := newMux(s)
	for _, rt := range append(append([]route{}, publicRoutes...), dataRoutes...) {
		t.Run(rt.pattern, func(t *testing.T) {
			if rt.handler == nil {
				t.Fatal("nil handler in the route table")
			}
			if rt.handler(s) == nil {
				t.Fatal("the route table produced a nil handler")
			}
			method, path := http.MethodGet, rt.pattern
			if i := strings.IndexByte(path, ' '); i >= 0 {
				method, path = path[:i], path[i+1:]
			}
			// Substitute something for a path parameter so the request actually routes.
			path = strings.ReplaceAll(path, "{id}", "winefridge")
			_, pattern := mux.Handler(httptest.NewRequest(method, path, nil))
			if pattern == "" {
				t.Errorf("%s %s is declared but the mux does not route it", method, path)
			}
		})
	}
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
