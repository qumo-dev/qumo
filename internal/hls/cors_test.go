package hls

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

// Which origins pass is the checker's (internal/cors). These cases are what
// the egress answers with: a page on an origin that isn't allowed must get no
// header that lets its browser hand it the response.
func Test_withCORS(t *testing.T) {
	const (
		app   = "https://app.example"
		other = "https://other.example"
	)

	tests := map[string]struct {
		allowed []string
		method  string
		origin  string
		// What the answer must carry.
		wantStatus      int
		wantAllowOrigin string
		wantServed      bool // the request reached the manifest handler
	}{
		"no origin: not a browser fetch":         {allowed: []string{app}, method: http.MethodGet, wantStatus: http.StatusOK, wantServed: true},
		"an allowed origin":                      {allowed: []string{app}, method: http.MethodGet, origin: app, wantStatus: http.StatusOK, wantAllowOrigin: app, wantServed: true},
		"an origin that isn't allowed":           {allowed: []string{app}, method: http.MethodGet, origin: other, wantStatus: http.StatusOK, wantServed: true},
		"nothing allowed: same-origin only":      {method: http.MethodGet, origin: other, wantStatus: http.StatusOK, wantServed: true},
		"nothing allowed, the egress's own page": {method: http.MethodGet, origin: "https://egress.example:8443", wantStatus: http.StatusOK, wantAllowOrigin: "https://egress.example:8443", wantServed: true},
		// The origin is echoed, never "*": the answer names who may read it.
		"any origin allowed":                             {allowed: []string{"*"}, method: http.MethodGet, origin: other, wantStatus: http.StatusOK, wantAllowOrigin: other, wantServed: true},
		"the null origin isn't allowed":                  {allowed: []string{app}, method: http.MethodGet, origin: "null", wantStatus: http.StatusOK, wantServed: true},
		"a preflight from an allowed origin":             {allowed: []string{app}, method: http.MethodOptions, origin: app, wantStatus: http.StatusNoContent, wantAllowOrigin: app},
		"a preflight from one that isn't":                {allowed: []string{app}, method: http.MethodOptions, origin: other, wantStatus: http.StatusNoContent},
		"a HEAD from an origin that isn't":               {allowed: []string{app}, method: http.MethodHead, origin: other, wantStatus: http.StatusOK, wantServed: true},
		"an allowed origin in different case":            {allowed: []string{app}, method: http.MethodGet, origin: "https://APP.example", wantStatus: http.StatusOK, wantServed: true},
		"an origin that only starts like an allowed one": {allowed: []string{app}, method: http.MethodGet, origin: app + ".evil.example", wantStatus: http.StatusOK, wantServed: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			served := false
			h := withCORS(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				served = true
				w.WriteHeader(http.StatusOK)
			}), tt.allowed)
			req := httptest.NewRequest(tt.method, "https://egress.example:8443/live/index.m3u8", nil)
			if tt.origin != "" {
				req.Header.Set("Origin", tt.origin)
			}
			rec := httptest.NewRecorder()

			h.ServeHTTP(rec, req)

			assert.Equal(t, tt.wantStatus, rec.Code)
			assert.Equal(t, tt.wantServed, served)
			assert.Equal(t, tt.wantAllowOrigin, rec.Header().Get("Access-Control-Allow-Origin"))
			if tt.wantAllowOrigin == "" {
				// Nothing that widens what a page may do with the answer.
				assert.Empty(t, rec.Header().Get("Access-Control-Allow-Headers"))
				assert.Empty(t, rec.Header().Get("Access-Control-Expose-Headers"))
				assert.Empty(t, rec.Header().Values("Vary"))
				return
			}
			// One origin's answer must not be served from a cache to another.
			assert.Equal(t, []string{"Origin"}, rec.Header().Values("Vary"))
			assert.Equal(t, "Range", rec.Header().Get("Access-Control-Allow-Headers"))
			assert.Equal(t, "Content-Length, Content-Range", rec.Header().Get("Access-Control-Expose-Headers"))
			// Credentials are never allowed: the egress takes none from a page.
			assert.Empty(t, rec.Header().Get("Access-Control-Allow-Credentials"))
		})
	}
}
