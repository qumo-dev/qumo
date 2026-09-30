package relay

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/qumo-dev/qumo/internal/credential"
)

// removedCredentialEnv are settings of the control-plane-signed credential
// model that ADR 0035 removed. A relay that still sets them is refused at
// startup rather than silently running with a different trust model.
var removedCredentialEnv = []string{"QUMO_RELAY_AUDIENCE", "QUMO_CREDENTIAL_ISSUER"}

// errUsageWithoutKeys refuses a relay that would report usage but authenticate
// no one: usage is reported per authenticated publisher, and a relay that
// used to introspect would otherwise come up open.
var errUsageWithoutKeys = errors.New("QUMO_CREDENTIAL_URL is set but QUMO_SIGNING_KEYS_FILE is not: " +
	"the relay verifies app-signed credentials against the keys in that file (qumo-deploy ADR 0035)")

// credentialAuth is how the relay authenticates publishers and reports usage.
type credentialAuth struct {
	// verifier checks publisher credentials against keys; nil leaves
	// publishers unauthenticated (open relay).
	verifier *credential.Verifier
	keys     credential.StaticKeys
	keysFile string
	// usage and meter report usage per admitted publisher; both are nil
	// unless QUMO_CREDENTIAL_URL is set.
	usage *usageClient
	meter *Meter
}

// newCredentialAuth reads the configuration from the environment:
//
//	QUMO_SIGNING_KEYS_FILE - JWK Set of the Ed25519 public keys whose
//	                         credentials the relay admits; unset disables
//	                         credential auth
//	QUMO_CREDENTIAL_URL    - control-plane base URL for usage reports;
//	                         requires QUMO_SIGNING_KEYS_FILE
func newCredentialAuth() (credentialAuth, error) {
	var removed []string
	for _, name := range removedCredentialEnv {
		if os.Getenv(name) != "" {
			removed = append(removed, name)
		}
	}
	if len(removed) > 0 {
		return credentialAuth{}, fmt.Errorf("%s no longer supported: credentials are app-signed and verified "+
			"against QUMO_SIGNING_KEYS_FILE (qumo-deploy ADR 0035); unset them", strings.Join(removed, ", "))
	}

	keysFile := os.Getenv("QUMO_SIGNING_KEYS_FILE")
	if keysFile == "" {
		if os.Getenv("QUMO_CREDENTIAL_URL") != "" {
			return credentialAuth{}, errUsageWithoutKeys
		}
		return credentialAuth{}, nil
	}

	keys, err := credential.LoadKeys(keysFile)
	if err != nil {
		return credentialAuth{}, fmt.Errorf("QUMO_SIGNING_KEYS_FILE: %w", err)
	}
	auth := credentialAuth{
		verifier: credential.NewVerifier(keys),
		keys:     keys,
		keysFile: keysFile,
	}
	if usage := newUsageClient(); usage != nil {
		auth.usage = usage
		auth.meter = newMeter(usage)
	}
	return auth, nil
}

// enabled reports whether publisher announcements must be authenticated.
func (a credentialAuth) enabled() bool {
	return a.verifier != nil
}
