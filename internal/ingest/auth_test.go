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
		app, streamKey  string
		wantPath        moqt.BroadcastPath
		wantCredentials []string
	}{
		"no credential":                   {app: "live", streamKey: "cam", wantPath: "/live/cam"},
		"on the stream key":               {app: "live", streamKey: "cam?jwt=k", wantPath: "/live/cam", wantCredentials: []string{"k"}},
		"on the app":                      {app: "live?jwt=a", streamKey: "cam", wantPath: "/live/cam", wantCredentials: []string{"a"}},
		"on both, the stream key's first": {app: "live?jwt=a", streamKey: "cam?jwt=k", wantPath: "/live/cam", wantCredentials: []string{"k", "a"}},
		"several, in order":               {app: "live", streamKey: "cam?jwt=k&jwt=abr", wantPath: "/live/cam", wantCredentials: []string{"k", "abr"}},
		"beside other parameters":         {app: "live", streamKey: "cam?x=1&jwt=k", wantPath: "/live/cam", wantCredentials: []string{"k"}},
		"a query that doesn't parse":      {app: "live", streamKey: "cam?jwt=%zz", wantPath: "/live/cam"},
		"an empty stream key":             {app: "live", streamKey: "", wantPath: "/live/"},
		"a query and no stream name":      {app: "live", streamKey: "?jwt=k", wantPath: "/live/", wantCredentials: []string{"k"}},
		"a nested app keeps its path":     {app: "acme/room", streamKey: "cam?jwt=k", wantPath: "/acme/room/cam", wantCredentials: []string{"k"}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			path, credentials := rtmpTarget(tt.app, tt.streamKey)

			assert.Equal(t, tt.wantPath, path)
			assert.Equal(t, tt.wantCredentials, credentials)
		})
	}
}

func TestCheckPushPath(t *testing.T) {
	for path, opt := range map[moqt.BroadcastPath]bool{
		"/live/cam":      false,
		"/.abr/live/cam": true,
		"/.hls/live/cam": true,
		"/.hlsx/cam":     false,
	} {
		err := checkPushPath(path)
		if opt {
			assert.ErrorIs(t, err, errOptInPath, path)
		} else {
			assert.NoError(t, err, path)
		}
	}
}

func TestAuthorizeAny(t *testing.T) {
	errFirst, errSecond := errors.New("first"), errors.New("second")
	lapses := time.Now().Add(time.Minute)
	tests := map[string]struct {
		credentials []string
		answers     map[string]error // by credential; absent is granted
		wantErr     error
		wantAsked   []string
	}{
		"none asks for a publish without one": {wantAsked: []string{""}},
		"the first granted":                   {credentials: []string{"a", "b"}, wantAsked: []string{"a"}},
		"a later one granted": {
			credentials: []string{"a", "b"}, answers: map[string]error{"a": errFirst}, wantAsked: []string{"a", "b"},
		},
		"none granted, the first refusal": {
			credentials: []string{"a", "b"}, answers: map[string]error{"a": errFirst, "b": errSecond},
			wantErr: errFirst, wantAsked: []string{"a", "b"},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			fake := &fakePublishAuthorizer{answer: func(ask publishAsk) (time.Time, error) {
				if err := tt.answers[ask.credential]; err != nil {
					return time.Time{}, err
				}
				return lapses, nil
			}}

			got, err := authorizeAny(fake.authorize, tt.credentials, "/live/cam")

			var asked []string
			for _, q := range fake.questions() {
				asked = append(asked, q.credential)
			}
			assert.Equal(t, tt.wantAsked, asked)
			if tt.wantErr != nil {
				assert.ErrorIs(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, lapses, got)
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

// TestPublishAuthorizer_OptIn verifies an opt-in's path is checked as its
// broadcast's against RELAY_PUBLISH, and granted only to a token whose key
// holds the opt-in in the key set, as a control plane sells it: a tenant's
// key whose entry lists "abr" signs for /.abr/live/, one without does not.
func TestPublishAuthorizer_OptIn(t *testing.T) {
	newVerifier := func(optIns ...token.OptIn) (*auth.Verifier, token.SigningKey) {
		key, err := token.GenerateKey("/live/")
		require.NoError(t, err)
		public := key.Public()
		public.OptIns = optIns
		set, err := token.MarshalKeySet(public)
		require.NoError(t, err)
		keys := filepath.Join(t.TempDir(), "keys.json")
		require.NoError(t, os.WriteFile(keys, set, 0o600))
		v, err := auth.NewVerifier(auth.VerifierConfig{Keys: keys})
		require.NoError(t, err)
		return v, key
	}
	paid, paidKey := newVerifier(token.OptInABR)
	unpaid, unpaidKey := newVerifier()
	optInGrant := token.Grant{Root: "/.abr/live/", Publish: []string{"/cam"}}
	sign := func(k token.SigningKey) string {
		credential, err := token.Sign(k, optInGrant, time.Minute)
		require.NoError(t, err)
		return credential
	}

	t.Run("a key that holds the opt-in", func(t *testing.T) {
		lapses, err := publishAuthorizer(paid, "/live/")(sign(paidKey), "/.abr/live/cam")
		require.NoError(t, err)
		assert.WithinDuration(t, time.Now().Add(time.Minute), lapses, 5*time.Second)
	})
	t.Run("a key that does not", func(t *testing.T) {
		_, err := publishAuthorizer(unpaid, "/live/")(sign(unpaidKey), "/.abr/live/cam")
		assert.Error(t, err)
	})
	t.Run("auth off: beneath RELAY_PUBLISH as its broadcast", func(t *testing.T) {
		_, err := publishAuthorizer(nil, "/live/")("", "/.abr/live/cam")
		assert.NoError(t, err)
	})
	t.Run("auth off: its broadcast outside RELAY_PUBLISH", func(t *testing.T) {
		_, err := publishAuthorizer(nil, "/live/")("", "/.abr/other/cam")
		assert.ErrorIs(t, err, errOutsidePublish)
	})
}

// TestPublishAuthorizer_Off verifies that without a key set or RELAY_PUBLISH
// there is no check at all.
func TestPublishAuthorizer_Off(t *testing.T) {
	assert.Nil(t, publishAuthorizer(nil, ""))
}
