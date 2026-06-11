package httpapi

import "net/http"

// corsMiddleware lets browser-based consumers (served from any origin, including
// file://) call the read-only API. It sets a permissive Access-Control-Allow-Origin
// and answers CORS preflight (OPTIONS) requests itself — before auth runs, since
// a preflight carries no Authorization header. Wildcard ACAO is safe here: the API
// authenticates via Bearer tokens, not cookies, so no Allow-Credentials is needed.
func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Add("Vary", "Origin")
		if r.Method == http.MethodOptions {
			w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
			w.Header().Set("Access-Control-Max-Age", "600")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
