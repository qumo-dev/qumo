package funnel

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestWithCORS(t *testing.T) {
	tests := map[string]struct {
		method      string
		origin      string
		wantStatus  int
		wantAllowed string
		wantServed  bool
	}{
		"allowed origin posts":        {method: http.MethodPost, origin: "https://app.example", wantStatus: http.StatusCreated, wantAllowed: "https://app.example", wantServed: true},
		"allowed origin preflights":   {method: http.MethodOptions, origin: "https://app.example", wantStatus: http.StatusNoContent, wantAllowed: "https://app.example"},
		"other origin gets no header": {method: http.MethodPost, origin: "https://evil.example", wantStatus: http.StatusCreated, wantServed: true},
		"no origin is not a browser":  {method: http.MethodPost, wantStatus: http.StatusCreated, wantServed: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			served := false
			h := withCORS(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				served = true
				w.WriteHeader(http.StatusCreated)
			}), []string{"https://app.example"})
			req := httptest.NewRequest(tt.method, "http://ingest.example/record", nil)
			if tt.origin != "" {
				req.Header.Set("Origin", tt.origin)
			}
			rr := httptest.NewRecorder()

			h.ServeHTTP(rr, req)

			assert.Equal(t, tt.wantStatus, rr.Code)
			assert.Equal(t, tt.wantAllowed, rr.Header().Get("Access-Control-Allow-Origin"))
			assert.Equal(t, tt.wantServed, served)
			if tt.wantAllowed != "" {
				assert.Contains(t, rr.Header().Get("Access-Control-Allow-Headers"), "Authorization")
				assert.Contains(t, rr.Header().Get("Access-Control-Allow-Methods"), "DELETE")
				assert.Equal(t, "Location", rr.Header().Get("Access-Control-Expose-Headers"))
			}
		})
	}
}
