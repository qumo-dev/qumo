package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseRelayURL(t *testing.T) {
	tests := map[string]struct {
		raw      string
		jwt      string
		wantLog  string
		wantDial string
		wantErr  string
	}{
		"no credential": {
			raw:      "moqt://localhost:9002/acme",
			wantLog:  "moqt://localhost:9002/acme",
			wantDial: "moqt://localhost:9002/acme",
		},
		"a credential": {
			raw:      "https://relay.example.com:4433/acme",
			jwt:      "h.p.s",
			wantLog:  "https://relay.example.com:4433/acme",
			wantDial: "https://relay.example.com:4433/acme?jwt=h.p.s",
		},
		"a query": {raw: "moqt://localhost:9002/acme?jwt=h.p.s", wantErr: "query"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := parseRelayURL(tt.raw, tt.jwt)

			if tt.wantErr != "" {
				assert.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantLog, got.String(), "the URL that is logged has no credential")
			assert.Equal(t, tt.wantDial, got.dialURL())
		})
	}
}

// TestParseRelayURL_QueryNotQuoted verifies a URL with a query is refused
// without quoting it, even one too malformed to parse: url.Parse's error would
// quote its whole input, credential included.
func TestParseRelayURL_QueryNotQuoted(t *testing.T) {
	_, err := parseRelayURL("moqt://[bad/acme?jwt=h.p.s", "")

	require.Error(t, err)
	assert.NotContains(t, err.Error(), "jwt=h.p.s")
}
