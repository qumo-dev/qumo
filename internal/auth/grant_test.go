package auth

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPattern_Covers(t *testing.T) {
	tests := map[string]struct {
		pattern string
		path    string
		want    bool
	}{
		"everything":                      {pattern: "**", path: "/any/where", want: true},
		"everything covers the root":      {pattern: "**", path: "/", want: true},
		"the base itself":                 {pattern: "acme/app/**", path: "/acme/app", want: true},
		"beneath the base":                {pattern: "acme/app/**", path: "/acme/app/room/1", want: true},
		"sibling sharing a string prefix": {pattern: "acme/app/**", path: "/acme/apple", want: false},
		"parent of the base":              {pattern: "acme/app/**", path: "/acme", want: false},
		"another tenant":                  {pattern: "acme/app/**", path: "/other/app", want: false},
		"case differs":                    {pattern: "acme/app/**", path: "/ACME/app", want: false},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			p, err := parsePattern(tt.pattern)
			require.NoError(t, err)

			got := p.covers(moqt.BroadcastPath(tt.path))

			assert.Equal(t, tt.want, got)
		})
	}
}

func TestParsePattern_RefusesUnsupported(t *testing.T) {
	for _, raw := range []string{
		"",          // empty
		"acme",      // an exact path: not a subtree
		"acme/*",    // a segment wildcard
		"*/chat/**", // a wildcard inside the base
		"/acme/**",  // a leading slash makes an empty segment
		"acme//app/**",
		"acme/../other/**",
		"acme/./app/**",
	} {
		t.Run(fmt.Sprintf("%q", raw), func(t *testing.T) {
			_, err := parsePattern(raw)

			assert.Error(t, err)
		})
	}
}

func TestParseGrant(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	tests := map[string]struct {
		body           string
		wantErr        error
		wantRefused    bool
		wantPublish    string // a path the grant must let publish, if set
		wantNotPublish string // a path it must not, if set
		wantExpires    time.Time
		wantRevalidate time.Duration
	}{
		"publish and subscribe with expiry": {
			body:           `{"publish":["acme/app/**"],"subscribe":["acme/**"],"expires":1000600,"revalidate":30}`,
			wantPublish:    "/acme/app/live",
			wantNotPublish: "/acme/other",
			wantExpires:    time.Unix(1_000_600, 0),
			wantRevalidate: 30 * time.Second,
		},
		"subscribe only": {
			body:           `{"subscribe":["**"]}`,
			wantNotPublish: "/acme/app",
		},
		"tier is ignored": {
			body:        `{"publish":["**"],"tier":"gold"}`,
			wantPublish: "/x",
		},
		"empty mounts are allowed":  {body: `{"publish":["**"],"mounts":{}}`, wantPublish: "/x"},
		"names nothing":             {body: `{"publish":[],"subscribe":[]}`, wantRefused: true},
		"no fields at all":          {body: `{}`, wantRefused: true},
		"expires already past":      {body: `{"publish":["**"],"expires":999999}`, wantErr: ErrInvalidGrant},
		"expires exactly now":       {body: `{"publish":["**"],"expires":1000000}`, wantErr: ErrInvalidGrant},
		"revalidate without expiry": {body: `{"publish":["**"],"revalidate":30}`, wantErr: ErrInvalidGrant},
		"zero revalidate":           {body: `{"publish":["**"],"expires":1000600,"revalidate":0}`, wantErr: ErrInvalidGrant},
		"root rewriting":            {body: `{"publish":["**"],"root":"acme"}`, wantErr: ErrInvalidGrant},
		"mounts":                    {body: `{"publish":["**"],"mounts":{".svc":".svc/p"}}`, wantErr: ErrInvalidGrant},
		"peer flag":                 {body: `{"publish":["**"],"peer":true}`, wantErr: ErrInvalidGrant},
		"non-subtree pattern":       {body: `{"publish":["acme/live"]}`, wantErr: ErrInvalidGrant},
		"not JSON":                  {body: `<html>`, wantErr: ErrInvalidGrant},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			g, err := parseGrant([]byte(tt.body), now)

			if tt.wantRefused {
				var refused RefusedError
				require.ErrorAs(t, err, &refused)
				assert.Equal(t, http.StatusForbidden, refused.Status)
				return
			}
			if tt.wantErr != nil {
				assert.ErrorIs(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			if tt.wantPublish != "" {
				assert.True(t, g.MayPublish(moqt.BroadcastPath(tt.wantPublish)))
			}
			if tt.wantNotPublish != "" {
				assert.False(t, g.MayPublish(moqt.BroadcastPath(tt.wantNotPublish)))
			}
			assert.Equal(t, tt.wantExpires, g.expires)
			assert.Equal(t, tt.wantRevalidate, g.revalidate)
		})
	}
}
