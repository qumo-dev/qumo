package relay

import (
	"testing"

	"github.com/qumo-dev/qumo/internal/credential"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewCredentialAuth(t *testing.T) {
	tests := map[string]struct {
		env          map[string]string
		wantErr      error
		wantErrText  string
		wantDisabled bool
		wantClient   bool
		wantLocal    bool
		wantIssuer   string
		wantJWKSURL  string
	}{
		"nothing configured": {
			env:          map[string]string{},
			wantDisabled: true,
		},
		"introspection (managed relays today)": {
			env:        map[string]string{"QUMO_CREDENTIAL_URL": "https://cp.example.com", "QUMO_RELAY_TOKEN": "t"}, //nolint:gosec // env var names, not credentials
			wantClient: true,
		},
		"local verification for a dev project": {
			env:         map[string]string{"QUMO_CREDENTIAL_URL": "api.example.com/", "QUMO_RELAY_AUDIENCE": "qumo-relay-dev"}, //nolint:gosec // env var names, not credentials
			wantLocal:   true,
			wantIssuer:  "https://api.example.com",
			wantJWKSURL: "https://api.example.com/v1/credentials/jwks",
		},
		"issuer override": {
			env: map[string]string{ //nolint:gosec // env var names, not credentials
				"QUMO_CREDENTIAL_URL":    "http://cp.internal:8080",
				"QUMO_CREDENTIAL_ISSUER": "https://api.example.com/",
				"QUMO_RELAY_AUDIENCE":    "qumo-relay-dev",
			},
			wantLocal:   true,
			wantIssuer:  "https://api.example.com",
			wantJWKSURL: "http://cp.internal:8080/v1/credentials/jwks",
		},
		"local verification needs the credential URL": {
			env:         map[string]string{"QUMO_RELAY_AUDIENCE": "qumo-relay-dev"},
			wantErrText: "QUMO_CREDENTIAL_URL",
		},
		"managed audience refused until the revocation feed": {
			env:     map[string]string{"QUMO_CREDENTIAL_URL": "https://cp.example.com", "QUMO_RELAY_AUDIENCE": credential.ManagedAudience, "QUMO_RELAY_TOKEN": "t"}, //nolint:gosec // env var names, not credentials
			wantErr: errManagedLocalVerification,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			for _, k := range []string{"QUMO_CREDENTIAL_URL", "QUMO_RELAY_TOKEN", "QUMO_RELAY_AUDIENCE", "QUMO_CREDENTIAL_ISSUER"} {
				t.Setenv(k, tt.env[k])
			}

			auth, err := newCredentialAuth()
			if tt.wantErr != nil {
				assert.ErrorIs(t, err, tt.wantErr)
				return
			}
			if tt.wantErrText != "" {
				assert.ErrorContains(t, err, tt.wantErrText)
				return
			}
			require.NoError(t, err)

			assert.Equal(t, !tt.wantDisabled, auth.enabled())
			assert.Equal(t, tt.wantClient, auth.client != nil)
			assert.Equal(t, tt.wantClient, auth.meter != nil, "only introspection mode meters usage")
			if !tt.wantLocal {
				assert.Nil(t, auth.verifier)
				assert.Nil(t, auth.jwks)
				return
			}
			require.NotNil(t, auth.verifier)
			require.NotNil(t, auth.jwks)
			assert.Equal(t, tt.wantIssuer, auth.issuer)
			assert.Equal(t, "qumo-relay-dev", auth.audience)
			assert.Equal(t, tt.wantJWKSURL, auth.jwks.URL())
			assert.Nil(t, auth.client, "local mode neither introspects nor reports usage")
		})
	}
}
