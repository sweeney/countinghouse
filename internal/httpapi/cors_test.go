package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// A CORS preflight (OPTIONS) must be answered without auth, with permissive
// headers, so browser consumers can call the API.
func TestCORS_Preflight(t *testing.T) {
	s, _ := dataSetup(t)
	req := httptest.NewRequest(http.MethodOptions, "/series", nil)
	req.Header.Set("Origin", "null")
	req.Header.Set("Access-Control-Request-Method", "GET")
	req.Header.Set("Access-Control-Request-Headers", "authorization")
	w := httptest.NewRecorder()
	s.handler().ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("preflight status = %d, want 204", w.Code)
	}
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("ACAO = %q, want *", got)
	}
	if got := w.Header().Get("Access-Control-Allow-Headers"); got == "" {
		t.Errorf("missing Access-Control-Allow-Headers")
	}
}

// A normal GET response must carry the ACAO header so the browser exposes it.
func TestCORS_GETHasACAO(t *testing.T) {
	s, _ := dataSetup(t)
	w := doGET(t, s, "/healthz")
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("ACAO = %q, want *", got)
	}
}
