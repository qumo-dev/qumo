package hls

import (
	"net/http"

	"github.com/qumo-dev/qumo/internal/cors"
)

// withCORS wraps h so browsers on another origin may read its responses.
//
// The player is served from its own origin (the playground's dev server, a web
// app in production) while the egress listens on a different port, so every
// manifest and segment fetch is cross-origin. Without these headers the browser
// receives the response and refuses to expose it — a 200 the page cannot read.
//
// Delivery policy belongs here rather than in the qumo-ledger stream handler:
// that package renders manifests and serves bytes, and stays independent of who
// is allowed to fetch them.
//
// Which origins are allowed is decided by [cors.NewChecker], the same rules the
// relay applies to WebTransport upgrades — including "*" and cors.SameHost — so
// one CORS_ALLOWED_ORIGINS setting governs both. What differs is the response:
// an upgrade is accepted or refused, whereas a fetch also needs the headers
// below before the browser will hand the body to the page.
func withCORS(h http.Handler, allowed []string) http.Handler {
	allow := cors.NewChecker(allowed)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The answer depends on the request's Origin whether or not it is
		// allowed, so every answer says so: a shared cache that kept one made
		// without the headers below must not serve it to an allowed origin,
		// whose browser would refuse it.
		w.Header().Add("Vary", "Origin")

		// A request with no Origin is not a browser fetch and needs no CORS
		// headers; the checker admits it for the same reason.
		if origin := r.Header.Get("Origin"); origin != "" && allow(r) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			// Players issue ranged segment reads, and need the length headers
			// to be readable to do so.
			w.Header().Set("Access-Control-Allow-Headers", "Range")
			w.Header().Set("Access-Control-Expose-Headers", "Content-Length, Content-Range")
		}

		// A preflight carries no body and must not reach the manifest renderer.
		if r.Method == http.MethodOptions {
			w.Header().Set("Access-Control-Allow-Methods", "GET, HEAD, OPTIONS")
			w.WriteHeader(http.StatusNoContent)
			return
		}

		h.ServeHTTP(w, r)
	})
}
