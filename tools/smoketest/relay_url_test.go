package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseRelayURL(t *testing.T) {
	tests := map[string]struct {
		raw      string
		wantLog  string
		wantDial string
	}{
		"no credential": {
			raw:      "moqt://localhost:9002/acme",
			wantLog:  "moqt://localhost:9002/acme",
			wantDial: "moqt://localhost:9002/acme",
		},
		"a credential": {
			raw:      "https://relay.example.com:4433/acme?jwt=h.p.s",
			wantLog:  "https://relay.example.com:4433/acme",
			wantDial: "https://relay.example.com:4433/acme?jwt=h.p.s",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := parseRelayURL(tt.raw)

			require.NoError(t, err)
			assert.Equal(t, tt.wantLog, got.String(), "the URL that is logged has no credential")
			assert.Equal(t, tt.wantDial, got.dialURL())
		})
	}
}

// TestParseRelayURL_MalformedHidesCredential verifies a URL that doesn't parse
// is reported without quoting it: url.Parse's error would quote its whole
// input, credential included.
func TestParseRelayURL_MalformedHidesCredential(t *testing.T) {
	_, err := parseRelayURL("moqt://[bad/acme?jwt=h.p.s")

	require.Error(t, err)
	assert.NotContains(t, err.Error(), "jwt=h.p.s")
}
