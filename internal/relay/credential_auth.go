package relay

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/qumo-dev/qumo/internal/credential"
)

// errManagedLocalVerification refuses QUMO_RELAY_AUDIENCE=qumo-relay: a
// managed relay verifying locally without the revocation feed would silently
// stop honoring revocation, so managed relays keep introspection until the
// feed consumer lands (qumo-dev/qumo#419).
var errManagedLocalVerification = errors.New(
	"QUMO_RELAY_AUDIENCE=" + credential.ManagedAudience + " is not supported yet: managed relays verify through " +
		"introspection until they consume the revocation feed (qumo-dev/qumo#419); leave QUMO_RELAY_AUDIENCE unset")

// credentialAuth is how the relay authenticates publishers and reports usage.
// At most one of client and verifier is used to admit announcements.
type credentialAuth struct {
	// client introspects credentials and reports usage (introspection mode).
	client *CredentialClient
	meter  *Meter
	// verifier checks credentials locally (local mode); jwks feeds it. issuer
	// and audience are what it holds a credential to.
	verifier *credential.Verifier
	jwks     *credential.JWKS
	issuer   string
	audience string
}

// newCredentialAuth selects the mode from the environment:
//
//	QUMO_CREDENTIAL_URL     - control-plane base URL; unset disables credential auth
//	QUMO_RELAY_AUDIENCE     - unset: introspection (managed relays today).
//	                          Set: verify locally against the JWKS and require
//	                          this audience; a customer's relay for a dev
//	                          project sets qumo-relay-dev.
//	QUMO_CREDENTIAL_ISSUER  - expected iss in local mode; defaults to
//	                          QUMO_CREDENTIAL_URL
//
// Local mode reads no revocation feed and reports no usage: a customer's relay
// cannot read the operator-authenticated feed, and dev traffic is not billed.
func newCredentialAuth() (credentialAuth, error) {
	audience := os.Getenv("QUMO_RELAY_AUDIENCE")
	if audience == "" {
		client := NewCredentialClient()
		if client == nil {
			return credentialAuth{}, nil
		}
		return credentialAuth{client: client, meter: newMeter(client)}, nil
	}
	if audience == credential.ManagedAudience {
		return credentialAuth{}, errManagedLocalVerification
	}

	baseURL := normalizeCredentialURL(os.Getenv("QUMO_CREDENTIAL_URL"))
	if baseURL == "" {
		return credentialAuth{}, fmt.Errorf("QUMO_RELAY_AUDIENCE is set but QUMO_CREDENTIAL_URL is not: " +
			"local verification fetches the control plane's JWKS from it")
	}
	issuer := normalizeCredentialURL(os.Getenv("QUMO_CREDENTIAL_ISSUER"))
	if issuer == "" {
		issuer = baseURL
	}

	jwks := credential.NewJWKS(baseURL, &http.Client{Timeout: 10 * time.Second})
	slog.Info("relay: verifying credentials locally; no revocation feed, so a revoked credential "+
		"stops working at its expiry, and no usage is reported",
		"jwks", jwks.URL(), "issuer", issuer, "audience", audience)
	return credentialAuth{
		verifier: credential.NewVerifier(jwks, issuer, audience),
		jwks:     jwks,
		issuer:   issuer,
		audience: audience,
	}, nil
}

// enabled reports whether publisher announcements must be authenticated.
func (a credentialAuth) enabled() bool {
	return a.client != nil || a.verifier != nil
}

// normalizeCredentialURL adds https:// when no scheme is given and drops a
// trailing slash, the same normalization NewCredentialClient applies.
func normalizeCredentialURL(u string) string {
	if u == "" {
		return ""
	}
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		u = "https://" + u
	}
	return strings.TrimRight(u, "/")
}
