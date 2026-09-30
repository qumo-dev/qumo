package credential

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPathAuth_Within pins check 4, prefix confinement: every granted path
// must equal the key's prefix or lie beneath it at a segment boundary.
func TestPathAuth_Within(t *testing.T) {
	str := func(s string) *string { return &s }
	const prefix = "tenant/project"
	tests := map[string]struct {
		grant   *pathAuth
		prefix  string
		wantErr error
	}{
		"root is the prefix":                {grant: &pathAuth{Root: "tenant/project", Pub: str("live")}, prefix: prefix},
		"root beneath the prefix":           {grant: &pathAuth{Root: "tenant/project/cams", Pub: str("")}, prefix: prefix},
		"grant equals the prefix":           {grant: &pathAuth{Root: "tenant/project", Sub: str("")}, prefix: prefix},
		"empty root, path in pub":           {grant: &pathAuth{Root: "", Pub: str("tenant/project/live")}, prefix: prefix},
		"slashes normalized":                {grant: &pathAuth{Root: "/tenant//project/", Pub: str("/live/")}, prefix: "/tenant/project/"},
		"publish and subscribe inside":      {grant: &pathAuth{Root: "tenant/project", Pub: str("a"), Sub: str("b")}, prefix: prefix},
		"no grant at all":                   {grant: &pathAuth{Root: "other"}, prefix: prefix},
		"no path_auth":                      {grant: nil, prefix: prefix},
		"unconstrained key":                 {grant: &pathAuth{Root: "", Pub: str("")}, prefix: ""},
		"another tenant":                    {grant: &pathAuth{Root: "other/project", Pub: str("live")}, prefix: prefix, wantErr: errOutsidePrefix},
		"sibling sharing the prefix string": {grant: &pathAuth{Root: "tenant/projectX", Pub: str("live")}, prefix: prefix, wantErr: errOutsidePrefix},
		"parent of the prefix":              {grant: &pathAuth{Root: "tenant", Pub: str("")}, prefix: prefix, wantErr: errOutsidePrefix},
		"the whole namespace":               {grant: &pathAuth{Root: "", Pub: str("")}, prefix: prefix, wantErr: errOutsidePrefix},
		"subscribe grant outside":           {grant: &pathAuth{Root: "tenant/project", Pub: str("live"), Sub: str("../../other")}, prefix: prefix, wantErr: errMalformed},
		"subscribe outside, no dots":        {grant: &pathAuth{Root: "", Pub: str("tenant/project/a"), Sub: str("other/b")}, prefix: prefix, wantErr: errOutsidePrefix},
		"dot-dot in root":                   {grant: &pathAuth{Root: "tenant/project/..", Pub: str("other")}, prefix: prefix, wantErr: errMalformed},
		"dot segment":                       {grant: &pathAuth{Root: "tenant/project/.", Pub: str("live")}, prefix: prefix, wantErr: errMalformed},
		"dot-dot even when unconstrained":   {grant: &pathAuth{Root: "a/..", Pub: str("")}, prefix: "", wantErr: errMalformed},
		"percent-encoded slash is literal":  {grant: &pathAuth{Root: "tenant%2Fproject", Pub: str("live")}, prefix: prefix, wantErr: errOutsidePrefix},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			err := tt.grant.within(tt.prefix)

			if tt.wantErr != nil {
				assert.ErrorIs(t, err, tt.wantErr)
				return
			}
			assert.NoError(t, err)
		})
	}
}

// TestVerifier_PrefixConfinement checks that the verifier applies the key's
// prefix: a key registered for one project cannot sign for another tenant,
// even for a path its own grant would cover.
func TestVerifier_PrefixConfinement(t *testing.T) {
	signer := newTestSigner(t, testKID)
	header := map[string]any{"alg": "EdDSA", "kid": testKID}
	grant := func(root, pub string) map[string]any {
		c := validClaims()
		c["path_auth"] = map[string]any{"root": root, "pub": pub}
		return c
	}
	verifier := &Verifier{
		keys: fakeKeys{testKID: {ID: testKID, Public: signer.pub, ProjectID: "p1", Prefix: "tenant/project"}},
		now:  func() time.Time { return testNow },
	}

	tests := map[string]struct {
		claims  map[string]any
		path    string
		wantErr error
	}{
		"own project":              {claims: grant("tenant/project", "live"), path: "/tenant/project/live"},
		"another tenant's path":    {claims: grant("victim/project", "live"), path: "/victim/project/live", wantErr: errOutsidePrefix},
		"the whole namespace":      {claims: grant("", ""), path: "/victim/project/live", wantErr: errOutsidePrefix},
		"inside but not announced": {claims: grant("tenant/project", "live"), path: "/tenant/project/other", wantErr: errPathNotCovered},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			cred, err := verifier.VerifyPublish(t.Context(), signer.sign(t, header, tt.claims), tt.path)

			if tt.wantErr != nil {
				assert.ErrorIs(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, "p1", cred.Key.ProjectID)
		})
	}
}
