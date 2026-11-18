package auth

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
)

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
