package hls

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"

	"github.com/qumo-dev/gomoqt/moqt"

	"github.com/qumo-dev/qumo/internal/auth"
	"github.com/qumo-dev/qumo/internal/envconfig"
	"github.com/qumo-dev/qumo/token"
)

// viewerAccess is how viewers' requests are let in: the query parameter a
// token travels in, carried onto every URL the manifests name, and the check
// it passes. The zero value lets everyone in and carries nothing.
type viewerAccess struct {
	// param is the query parameter of a manifest's URL players repeat on
	// every request it leads to (HLS EXT-X-DEFINE:QUERYPARAM, DASH
	// UrlQueryInfo), "" for none.
	param string
	// check decides whether credential, the value of param, may watch the
	// broadcast at path; nil lets every request in.
	check func(credential string, path moqt.BroadcastPath) error
}

// errNoViewerCredential refuses a request that carried no token while viewers
// are checked.
var errNoViewerCredential = errors.New("hls: no credential")

// viewerAccessFromEnv reads how viewers are let in:
//
//	QUMO_AUTH_KEYS       - the key set viewers' tokens are verified against, as the relay reads it: every request needs a token that may subscribe to its broadcast (default: none, every viewer is let in)
//	QUMO_AUTH_KEYS_CACHE - a file a downloaded key set is kept in between runs
//	QUMO_RELAY_TOKEN     - bearer token sent to a key-set URL
//	HLS_TOKEN_PARAM      - the query parameter a viewer's token travels in, carried onto every URL the manifests name (default "jwt" with QUMO_AUTH_KEYS, else none: set it for a CDN that checks its own tokens); an HLS variable name: letters, digits, '-' and '_'
//
// The key set is refreshed until ctx ends.
func viewerAccessFromEnv(ctx context.Context) (viewerAccess, string, error) {
	keys := os.Getenv("QUMO_AUTH_KEYS")
	defaultParam := ""
	if keys != "" {
		defaultParam = "jwt"
	}
	access := viewerAccess{param: envconfig.String("HLS_TOKEN_PARAM", defaultParam)}
	if access.param != "" && !isVariableName(access.param) {
		return viewerAccess{}, "", fmt.Errorf("hls: HLS_TOKEN_PARAM %q is not an HLS variable name (letters, digits, '-', '_')", access.param)
	}
	if keys == "" {
		return access, "off: every viewer is let in", nil
	}
	v, err := auth.NewVerifier(auth.VerifierConfig{
		Keys:      keys,
		KeysCache: os.Getenv("QUMO_AUTH_KEYS_CACHE"),
		Token:     os.Getenv("QUMO_RELAY_TOKEN"),
	})
	if err != nil {
		return viewerAccess{}, "", fmt.Errorf("hls: %w", err)
	}
	go v.Run(ctx)
	access.check = subscribeCheck(v)
	return access, v.Source(), nil
}

// isVariableName reports whether s is an HLS variable name (RFC 8216bis
// EXT-X-DEFINE): one or more letters, digits, '-' or '_'. The manifests
// declare the token parameter as one and splice it into every URI they name.
func isVariableName(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

// subscribeCheck returns the check a viewer's token passes: v verifies it,
// and it may subscribe to the broadcast. It is checked on every request, so a
// token stops letting its viewer in once it expires or its key leaves the set.
func subscribeCheck(v *auth.Verifier) func(credential string, path moqt.BroadcastPath) error {
	return func(credential string, path moqt.BroadcastPath) error {
		if credential == "" {
			return errNoViewerCredential
		}
		g, err := v.Check(credential, token.ActionSubscribe, path)
		if err != nil {
			return err
		}
		if !g.Allows(token.ActionSubscribe, path) {
			return refused(http.StatusForbidden, "hls: the credential may not watch "+string(path))
		}
		return nil
	}
}

// refused is a refusal answered with status.
func refused(status int, reason string) error {
	return fmt.Errorf("%s: %w", reason, auth.RefusedError{Status: status})
}

// admit reports whether the request r may be answered for the broadcast at
// path, answering it itself when not: 401 for a missing or invalid token, 403
// for one that may not watch path, 503 while tokens can't be checked (no key
// set loaded). A response it admits while viewers are checked is marked
// private, so a shared cache in front never serves it to another viewer.
func (a viewerAccess) admit(w http.ResponseWriter, r *http.Request, path string) bool {
	if a.check == nil {
		return true
	}
	if err := a.check(r.URL.Query().Get(a.param), moqt.BroadcastPath(path)); err != nil {
		status := auth.RefusalStatus(err)
		if errors.Is(err, errNoViewerCredential) {
			status = http.StatusUnauthorized
		}
		w.Header().Set("Cache-Control", "no-store")
		http.Error(w, http.StatusText(status), status)
		return false
	}
	w.Header().Set("Cache-Control", "private")
	return true
}

// keepQuery returns target with r's query, so a redirect keeps the token a
// viewer's request carried.
func keepQuery(target string, r *http.Request) string {
	if r.URL.RawQuery == "" {
		return target
	}
	return target + "?" + r.URL.RawQuery
}
