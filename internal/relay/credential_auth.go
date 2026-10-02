package relay

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/qumo-dev/qumo/internal/credential"
	"github.com/qumo-dev/qumo/internal/trust"
)

var (
	errBothTrustSources = errors.New("QUMO_SIGNING_KEYS_FILE and QUMO_CREDENTIAL_URL are both set: " +
		"a relay trusts either static keys (self-hosted) or the control plane's trust snapshot (managed), not both")
	errNoRelayToken = errors.New("QUMO_CREDENTIAL_URL is set but QUMO_RELAY_TOKEN is not: " +
		"a managed relay authenticates to the control plane with it")
)

// credentialAuth is how the relay authenticates publishers and reports usage.
// It has one of two trust sources, or none (open relay):
//
//   - static: keys from QUMO_SIGNING_KEYS_FILE, never changing; no qumo calls.
//   - managed: the control plane's trust snapshot, polled; usage reported.
type credentialAuth struct {
	// verifier checks publisher credentials; nil leaves publishers
	// unauthenticated (open relay).
	verifier *credential.Verifier

	// Static mode.
	keys     credential.StaticKeys
	keysFile string

	// Managed mode.
	trust  *trust.Store
	poller *trust.Poller
	usage  *usageClient
	meter  *Meter
}

// newCredentialAuth reads the configuration from the environment:
//
//	QUMO_SIGNING_KEYS_FILE - JWK Set of the Ed25519 public keys whose
//	                         credentials the relay admits (self-hosted relay)
//	QUMO_CREDENTIAL_URL    - control-plane base URL: poll the trust snapshot
//	                         and report usage (managed relay)
//	QUMO_RELAY_TOKEN       - bearer token for the control plane; required
//	                         with QUMO_CREDENTIAL_URL
//
// Neither set leaves the relay open.
func newCredentialAuth() (credentialAuth, error) {
	keysFile := os.Getenv("QUMO_SIGNING_KEYS_FILE")
	usage := newUsageClient()
	switch {
	case keysFile != "" && usage != nil:
		return credentialAuth{}, errBothTrustSources
	case keysFile != "":
		keys, err := credential.LoadKeys(keysFile)
		if err != nil {
			return credentialAuth{}, fmt.Errorf("QUMO_SIGNING_KEYS_FILE: %w", err)
		}
		return credentialAuth{
			verifier: credential.NewVerifier(keys),
			keys:     keys,
			keysFile: keysFile,
		}, nil
	case usage != nil:
		if usage.authToken == "" {
			return credentialAuth{}, errNoRelayToken
		}
		store := trust.NewStore()
		return credentialAuth{
			verifier: credential.NewVerifier(store),
			trust:    store,
			poller:   trust.NewPoller(usage.baseURL, usage.authToken, &http.Client{Timeout: 10 * time.Second}, store),
			usage:    usage,
			meter:    newMeter(usage),
		}, nil
	default:
		return credentialAuth{}, nil
	}
}

// enabled reports whether publisher announcements must be authenticated.
func (a credentialAuth) enabled() bool {
	return a.verifier != nil
}
