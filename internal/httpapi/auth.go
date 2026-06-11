package httpapi

import (
	"net/http"
	"strings"

	"github.com/sweeney/identity/common/auth"
)

// authMiddleware returns a middleware that validates Bearer JWTs when
// IdentityURL is set. When IdentityURL is empty it returns a no-op wrapper so
// existing tests and local runs work without auth configured.
//
// The wrapper is reusable: a later milestone wraps each data route with it.
func (s *Server) authMiddleware() func(http.Handler) http.Handler {
	if s.IdentityURL == "" {
		return func(h http.Handler) http.Handler { return h }
	}
	verifier, err := auth.NewJWKSVerifier(auth.JWKSVerifierConfig{
		IssuerURL: s.IdentityURL,
		Issuer:    s.IdentityURL,
		Logger:    s.Logger,
	})
	if err != nil {
		// Only fails when IssuerURL or Issuer is empty, guarded above.
		panic(err)
	}
	s.verifier = verifier
	return func(h http.Handler) http.Handler { return requireAuth(verifier, h) }
}

// requireAuth rejects requests lacking a valid Bearer JWT. Unlike statehouse
// (which only accepted user tokens via Parse), countinghouse accepts BOTH:
//   - a user token (Parse succeeds and the user is active), OR
//   - a service token (ParseServiceToken succeeds — a successful parse of a
//     client_credentials token is itself the validity signal; there is no
//     IsActive flag on service tokens).
//
// This is the explicit fix for statehouse's service-token gap.
func requireAuth(verifier *auth.JWKSVerifier, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reject := func() {
			w.Header().Set("WWW-Authenticate", `Bearer realm="`+verifier.Issuer()+`"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
		}
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || token == "" {
			reject()
			return
		}
		// User token: valid and active.
		if c, err := verifier.Parse(r.Context(), token); err == nil && c.IsActive {
			next.ServeHTTP(w, r)
			return
		}
		// Service token: a successful parse is the validity signal.
		if _, err := verifier.ParseServiceToken(r.Context(), token); err == nil {
			next.ServeHTTP(w, r)
			return
		}
		reject()
	})
}
