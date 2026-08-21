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

// Timing-Allow-Origin opts cross-origin consumers into the real numbers in
// PerformanceResourceTiming. Without it a browser still records an entry for the
// request, but zeroes every phase (DNS, TCP, TLS, TTFB) and the transfer sizes,
// so a consumer measuring countinghouse can see only total duration.
//
// It is a SEPARATE opt-in from CORS — ACAO does not imply it — so it is asserted
// on its own rather than assumed to ride along with the CORS headers.
func TestCORS_TimingAllowOrigin(t *testing.T) {
	s, _ := dataSetup(t)

	// The real response is the one the browser attaches timing to.
	w := doGET(t, s, "/healthz")
	if got := w.Header().Get("Timing-Allow-Origin"); got != "*" {
		t.Errorf("TAO on GET = %q, want *", got)
	}

	// Set on the preflight too. It does nothing there, but a header that appears
	// only on some responses invites the question of which — and the answer is
	// "always", so pin it.
	req := httptest.NewRequest(http.MethodOptions, "/series", nil)
	req.Header.Set("Origin", "null")
	req.Header.Set("Access-Control-Request-Method", "GET")
	pre := httptest.NewRecorder()
	s.handler().ServeHTTP(pre, req)
	if got := pre.Header().Get("Timing-Allow-Origin"); got != "*" {
		t.Errorf("TAO on preflight = %q, want *", got)
	}
}
