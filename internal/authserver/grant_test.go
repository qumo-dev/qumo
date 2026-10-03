package authserver

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func ptr(s string) *string { return &s }

func TestGrantFor(t *testing.T) {
	exp := float64(time.Unix(1_800_000_600, 0).Unix())
	tests := map[string]struct {
		prefix        string
		pathAuth      pathAuth
		wantPublish   []string
		wantSubscribe []string
		wantStatus    int // non-zero: refused with this status
	}{
		"publish and subscribe": {
			pathAuth:      pathAuth{Root: "acme/app", Pub: ptr("alice"), Sub: ptr("")},
			wantPublish:   []string{"acme/app/alice/**"},
			wantSubscribe: []string{"acme/app/**"},
		},
		"subscribe only": {
			pathAuth:      pathAuth{Root: "acme/app", Sub: ptr("live")},
			wantSubscribe: []string{"acme/app/live/**"},
		},
		"slashes normalized": {
			pathAuth:    pathAuth{Root: "/acme//app/", Pub: ptr("/alice/")},
			wantPublish: []string{"acme/app/alice/**"},
		},
		"within the key's prefix": {
			prefix:      "acme/app",
			pathAuth:    pathAuth{Root: "acme/app", Pub: ptr("alice")},
			wantPublish: []string{"acme/app/alice/**"},
		},
		"exactly the key's prefix": {
			prefix:      "acme/app",
			pathAuth:    pathAuth{Root: "acme/app", Pub: ptr("")},
			wantPublish: []string{"acme/app/**"},
		},
		"another tenant's path": {
			prefix:     "acme/app",
			pathAuth:   pathAuth{Root: "other/app", Pub: ptr("")},
			wantStatus: http.StatusForbidden,
		},
		"a sibling sharing a string prefix": {
			prefix:     "acme/app",
			pathAuth:   pathAuth{Root: "acme/apple", Pub: ptr("")},
			wantStatus: http.StatusForbidden,
		},
		"a parent of the key's prefix": {
			prefix:     "acme/app",
			pathAuth:   pathAuth{Root: "acme", Sub: ptr("")},
			wantStatus: http.StatusForbidden,
		},
		"only sub escapes the prefix": {
			prefix:     "acme/app",
			pathAuth:   pathAuth{Root: "acme/app", Pub: ptr("alice"), Sub: ptr("../../other")},
			wantStatus: http.StatusUnauthorized,
		},
		"dot-dot in root": {
			pathAuth:   pathAuth{Root: "acme/app/../../other", Pub: ptr("")},
			wantStatus: http.StatusUnauthorized,
		},
		"a wildcard segment": {
			pathAuth:   pathAuth{Root: "acme/*", Pub: ptr("")},
			wantStatus: http.StatusUnauthorized,
		},
		"names no path": {
			pathAuth:   pathAuth{Root: "", Pub: ptr("")},
			wantStatus: http.StatusUnauthorized,
		},
		"grants nothing": {
			pathAuth:   pathAuth{Root: "acme/app"},
			wantStatus: http.StatusForbidden,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			pa := tt.pathAuth
			c := claims{PathAuth: &pa, ExpiresAt: &exp}

			g, err := grantFor(c, Key{Prefix: tt.prefix}, 30*time.Second)

			if tt.wantStatus != 0 {
				var refused refusal
				require.ErrorAs(t, err, &refused)
				assert.Equal(t, tt.wantStatus, refused.status)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantPublish, g.Publish)
			assert.Equal(t, tt.wantSubscribe, g.Subscribe)
			assert.Equal(t, int64(1_800_000_660), g.Expires, "exp plus the leeway")
			assert.Equal(t, int64(30), g.Revalidate)
		})
	}
}
