package credential

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCredential_Within pins the no-widening rule for a refresh: every grant
// of the new credential lies at or beneath the same role's grant of the old.
func TestCredential_Within(t *testing.T) {
	str := func(s string) *string { return &s }
	cred := func(root string, pub, sub *string) Credential {
		return Credential{grants: pathAuth{Root: root, Pub: pub, Sub: sub}}
	}
	prev := cred("t/p", str("live"), str("chat"))

	tests := map[string]struct {
		next Credential
		want bool
	}{
		"same grants":                      {next: cred("t/p", str("live"), str("chat")), want: true},
		"narrower publish":                 {next: cred("t/p", str("live/cam1"), str("chat")), want: true},
		"drops the subscribe grant":        {next: cred("t/p", str("live"), nil), want: true},
		"same scope, different root split": {next: cred("t", str("p/live"), str("p/chat")), want: true},
		"wider publish":                    {next: cred("t/p", str(""), str("chat")), want: false},
		"sibling publish":                  {next: cred("t/p", str("lively"), str("chat")), want: false},
		"publish moved to subscribe scope": {next: cred("t/p", str("chat"), str("chat")), want: false},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.next.Within(prev))
		})
	}

	t.Run("a role the previous credential lacked", func(t *testing.T) {
		pubOnly := cred("t/p", str("live"), nil)

		assert.False(t, cred("t/p", str("live"), str("live")).Within(pubOnly))
	})
}

func TestVerifier_Verify_ExpiresAt(t *testing.T) {
	signer := newTestSigner(t, testKID)
	token := signer.sign(t, map[string]any{"alg": "EdDSA", "kid": testKID}, validClaims())

	cred, err := newTestVerifier(signer).Verify(t.Context(), token)

	require.NoError(t, err)
	assert.Equal(t, testNow.Add(10*time.Minute), cred.ExpiresAt)
	assert.True(t, cred.CoversPublish("/tenant/project/live"))
}
