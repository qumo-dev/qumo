package ingest

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/qumo-dev/qumo/internal/auth"
	"github.com/qumo-dev/qumo/token"
)

func TestRTMPTarget(t *testing.T) {
	tests := map[string]struct {
		app, streamKey string
		wantPath       moqt.BroadcastPath
		wantCredential string
	}{
		"no credential":               {app: "live", streamKey: "cam", wantPath: "/live/cam"},
		"on the stream key":           {app: "live", streamKey: "cam?jwt=k", wantPath: "/live/cam", wantCredential: "k"},
		"on the app":                  {app: "live?jwt=a", streamKey: "cam", wantPath: "/live/cam", wantCredential: "a"},
		"on both, the stream key's":   {app: "live?jwt=a", streamKey: "cam?jwt=k", wantPath: "/live/cam", wantCredential: "k"},
		"beside other parameters":     {app: "live", streamKey: "cam?x=1&jwt=k", wantPath: "/live/cam", wantCredential: "k"},
		"a query that doesn't parse":  {app: "live", streamKey: "cam?jwt=%zz", wantPath: "/live/cam"},
		"an empty stream key":         {app: "live", streamKey: "", wantPath: "/live/"},
		"a query and no stream name":  {app: "live", streamKey: "?jwt=k", wantPath: "/live/", wantCredential: "k"},
		"a nested app keeps its path": {app: "acme/room", streamKey: "cam?jwt=k", wantPath: "/acme/room/cam", wantCredential: "k"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			path, credential := rtmpTarget(tt.app, tt.streamKey)

			assert.Equal(t, tt.wantPath, path)
			assert.Equal(t, tt.wantCredential, credential)
		})
	}
}

// newKeySetVerifier returns a Verifier trusting a fresh key that pauses
// pause, and the key.
func newKeySetVerifier(tb testing.TB, pause ...token.Action) (*auth.Verifier, token.SigningKey) {
	tb.Helper()
	key, err := token.GenerateKey("")
	require.NoError(tb, err)
	public := key.Public()
	public.Pause = pause
	set, err := token.MarshalKeySet(public)
	require.NoError(tb, err)
	keys := filepath.Join(tb.TempDir(), "keys.json")
	require.NoError(tb, os.WriteFile(keys, set, 0o600))
	v, err := auth.NewVerifier(auth.VerifierConfig{Keys: keys})
	require.NoError(tb, err)
	return v, key
}

func TestPublishAuthorizer(t *testing.T) {
	v, key := newKeySetVerifier(t)
	paused, pausedKey := newKeySetVerifier(t, token.ActionPublish)
	sign := func(k token.SigningKey, g token.Grant) string {
		credential, err := token.Sign(k, g, time.Minute)
		require.NoError(t, err)
		return credential
	}
	publishLive := token.Grant{Root: "/live/", Publish: []string{"/cam"}}
	tests := map[string]struct {
		verifier   *auth.Verifier
		beneath    string
		credential string
		path       moqt.BroadcastPath
		wantErr    error
		wantStatus int
		wantLapse  bool
	}{
		"a credential that may publish": {verifier: v, credential: sign(key, publishLive), path: "/live/cam", wantLapse: true},
		"no credential":                 {verifier: v, path: "/live/cam", wantErr: errNoCredential},
		"an invalid credential": {
			verifier: v, credential: "not-a-token", path: "/live/cam", wantStatus: http.StatusUnauthorized,
		},
		"a credential for another path": {
			verifier: v, credential: sign(key, publishLive), path: "/live/other",
		},
		"a credential that only subscribes": {
			verifier: v, credential: sign(key, token.Grant{Root: "/live/", Subscribe: []string{"/cam"}}), path: "/live/cam",
		},
		"a key paused for publishing": {
			verifier: paused, credential: sign(pausedKey, publishLive), path: "/live/cam", wantStatus: http.StatusForbidden,
		},
		"auth off, beneath RELAY_PUBLISH": {beneath: "/live/", path: "/live/cam"},
		"auth off, outside RELAY_PUBLISH": {beneath: "/live/", path: "/other/cam", wantErr: errOutsidePublish},
		"outside RELAY_PUBLISH, before the credential": {
			verifier: v, beneath: "/other/", credential: sign(key, publishLive), path: "/live/cam", wantErr: errOutsidePublish,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			authorize := publishAuthorizer(tt.verifier, tt.beneath)
			require.NotNil(t, authorize)

			lapses, err := authorize(tt.credential, tt.path)

			switch {
			case tt.wantErr != nil:
				assert.ErrorIs(t, err, tt.wantErr)
			case tt.wantStatus != 0:
				refused, ok := errors.AsType[auth.RefusedError](err)
				require.True(t, ok, "a refusal, got %v", err)
				assert.Equal(t, tt.wantStatus, refused.Status)
			case tt.wantLapse:
				require.NoError(t, err)
				assert.WithinDuration(t, time.Now().Add(time.Minute), lapses, 5*time.Second, "cut when the credential expires")
			case tt.verifier == nil:
				require.NoError(t, err)
				assert.True(t, lapses.IsZero(), "auth off never cuts")
			default:
				assert.Error(t, err)
			}
		})
	}
}

// TestPublishAuthorizer_Off verifies that without a key set or RELAY_PUBLISH
// there is no check at all.
func TestPublishAuthorizer_Off(t *testing.T) {
	assert.Nil(t, publishAuthorizer(nil, ""))
}
