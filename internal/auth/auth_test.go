package auth

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCheckURL(t *testing.T) {
	tests := map[string]struct {
		url         string
		wantErrText string
	}{
		"https":             {url: "https://keys.example.com/v1/set"},
		"loopback http":     {url: "http://127.0.0.1:4440/"},
		"localhost http":    {url: "http://localhost:4440/"},
		"http off loopback": {url: "http://keys.example.com/", wantErrText: "loopback"},
		"another scheme":    {url: "ftp://keys.example.com/", wantErrText: "https"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			err := checkURL(tt.url)

			if tt.wantErrText != "" {
				assert.ErrorContains(t, err, tt.wantErrText)
				return
			}
			assert.NoError(t, err)
		})
	}
}

func TestRefusalStatus(t *testing.T) {
	tests := map[string]struct {
		err  error
		want int
	}{
		"a refusal":         {err: RefusedError{Status: http.StatusForbidden}, want: http.StatusForbidden},
		"a wrapped refusal": {err: fmt.Errorf("connect: %w", RefusedError{Status: http.StatusUnauthorized}), want: http.StatusUnauthorized},
		"any other error":   {err: errors.New("boom"), want: http.StatusServiceUnavailable},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got := RefusalStatus(tt.err)

			assert.Equal(t, tt.want, got)
		})
	}
}
