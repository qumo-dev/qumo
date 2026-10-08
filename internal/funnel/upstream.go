package funnel

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"time"

	"github.com/qumo-dev/gomoqt/moqt"

	"github.com/qumo-dev/qumo/internal/tlsclient"
	"github.com/qumo-dev/qumo/token"
)

// upstreamRetryInterval is how long the funnel waits before dialing the relay
// again after a session could not be opened or ended.
const upstreamRetryInterval = 2 * time.Second

// upstream is the funnel's session to a relay, through which the relay
// subscribes to the tracks the funnel publishes.
type upstream struct {
	// url is the relay's URL. It may carry a credential as ?jwt=.
	url string
	// tls is the client TLS configuration for the relay.
	tls *tls.Config
	// key, when set, signs a credential for each session that grants
	// publishing at publish, replacing any ?jwt= in url.
	key     *token.SigningKey
	publish string
}

// RelayConfig is how the funnel reaches the relay it publishes through.
type RelayConfig struct {
	// URL is the relay's URL, https:// for WebTransport or moqt:// for native
	// QUIC. It may carry a credential as ?jwt=.
	URL string
	// CAFile is a PEM certificate to trust as the relay's root; empty trusts
	// the system roots. Insecure skips verification, for a self-signed dev
	// relay.
	CAFile   string
	Insecure bool
	// SigningKeyFile and Publish, set together, make the funnel sign a fresh
	// credential granting publishing at Publish for every session, in place
	// of any ?jwt= in URL.
	SigningKeyFile string
	Publish        string
}

// PublishThroughRelay keeps a session to the relay open until ctx ends,
// dialing again whenever one ends, so the relay can subscribe to the
// broadcasts published on mux. It returns early only when cfg is unusable.
func PublishThroughRelay(ctx context.Context, cfg RelayConfig, mux *moqt.TrackMux) error {
	u, err := newUpstream(cfg)
	if err != nil {
		return err
	}
	u.run(ctx, mux)
	return nil
}

// newUpstream checks cfg and loads what the sessions need.
func newUpstream(cfg RelayConfig) (*upstream, error) {
	if _, err := url.Parse(cfg.URL); err != nil {
		// url.Parse's *url.Error quotes the whole URL, and its query may
		// carry a credential; report only the reason it wraps.
		if ue, ok := errors.AsType[*url.Error](err); ok {
			err = ue.Err
		}
		return nil, fmt.Errorf("RELAY_URL: invalid URL: %w", err)
	}
	tc := &tls.Config{MinVersion: tls.VersionTLS13}
	if err := tlsclient.Apply(tc, cfg.CAFile, cfg.Insecure); err != nil {
		return nil, fmt.Errorf("relay TLS: %w", err)
	}
	u := &upstream{url: cfg.URL, tls: tc}
	switch {
	case cfg.SigningKeyFile == "" && cfg.Publish == "":
	case cfg.SigningKeyFile == "" || cfg.Publish == "":
		return nil, errors.New("RELAY_SIGNING_KEY and RELAY_PUBLISH must be set together")
	default:
		key, err := token.LoadSigningKey(cfg.SigningKeyFile)
		if err != nil {
			return nil, fmt.Errorf("RELAY_SIGNING_KEY: %w", err)
		}
		// Signing one now refuses a grant outside the key's prefix at startup
		// rather than at the first dial.
		if _, err := token.Sign(key, publishGrant(cfg.Publish), token.Options{TTL: token.MaxLifetime}); err != nil {
			return nil, fmt.Errorf("RELAY_PUBLISH: %w", err)
		}
		u.key, u.publish = &key, cfg.Publish
	}
	return u, nil
}

// publishGrant grants publishing every track of the broadcast at path and of
// every broadcast beneath it.
func publishGrant(path string) token.Grant {
	return token.Grant{Scopes: []token.Scope{{Actions: []token.Action{token.ActionPublish}, Broadcast: path, Prefix: true}}}
}

// run keeps a session to the relay open until ctx ends, dialing again after
// each one ends, with a signed credential fresh on every dial.
func (u *upstream) run(ctx context.Context, mux *moqt.TrackMux) {
	for {
		sess, err := u.dial(ctx, mux)
		switch {
		case err == nil:
			slog.Info("funnel: publishing through the relay")
			select {
			case <-ctx.Done():
				// not actionable: the funnel is stopping.
				_ = sess.CloseWithError(moqt.NoError, "funnel stopping")
				return
			case <-sess.Context().Done():
				slog.Warn("funnel: the relay session ended, dialing again", "cause", moqt.Cause(sess.Context()))
			}
		case ctx.Err() == nil:
			slog.Warn("funnel: cannot reach the relay, retrying", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(upstreamRetryInterval):
		}
	}
}

// dial opens one session to the relay, serving mux's broadcasts on it.
func (u *upstream) dial(ctx context.Context, mux *moqt.TrackMux) (*moqt.Session, error) {
	target, err := u.target()
	if err != nil {
		return nil, err
	}
	sess, err := (&moqt.Dialer{TLSConfig: u.tls.Clone()}).Dial(ctx, target, mux)
	if err != nil {
		return nil, fmt.Errorf("dial relay: %w", err)
	}
	return sess, nil
}

// target returns the URL to dial, with a freshly signed credential when the
// upstream signs its own.
func (u *upstream) target() (string, error) {
	if u.key == nil {
		return u.url, nil
	}
	credential, err := token.Sign(*u.key, publishGrant(u.publish), token.Options{TTL: token.MaxLifetime})
	if err != nil {
		return "", fmt.Errorf("sign relay credential: %w", err)
	}
	parsed, err := url.Parse(u.url)
	if err != nil {
		return "", err
	}
	query := parsed.Query()
	query.Set("jwt", credential)
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}
