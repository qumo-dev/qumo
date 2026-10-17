// Package upstream keeps a session to a relay open, through which the relay
// subscribes to the broadcasts a qumo process publishes on its own TrackMux.
// The funnel and the RTMP and RTSP ingest servers publish through it.
package upstream

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/qumo-dev/gomoqt/moqt"

	"github.com/qumo-dev/qumo/internal/envconfig"
	"github.com/qumo-dev/qumo/internal/relaycred"
	"github.com/qumo-dev/qumo/internal/tlsclient"
	"github.com/qumo-dev/qumo/token"
)

// retryInterval is how long an Upstream waits before dialing the relay again
// after a session could not be opened or ended.
const retryInterval = 2 * time.Second

// Upstream is a publisher's session to a relay, through which the relay
// subscribes to the tracks the publisher serves.
type Upstream struct {
	// name says who publishes, in the log.
	name string
	// url is the relay's URL. It may carry a credential as ?jwt=.
	url string
	// tls is the client TLS configuration for the relay.
	tls *tls.Config
	// quic is the QUIC configuration of the session; nil is quic-go's
	// defaults.
	quic *quic.Config
	// key, when set, signs a credential for each session that grants
	// publishing beneath publish, a directory, replacing any ?jwt= in url,
	// and a fresh one on the session before its grant lapses.
	key     *token.SigningKey
	publish string
}

// Config is how a publisher reaches the relay it publishes through.
type Config struct {
	// Name says who publishes, in the log: "funnel", "rtmp".
	Name string
	// URL is the relay's URL, https:// for WebTransport or moqt:// for native
	// QUIC. It may carry a credential as ?jwt=.
	URL string
	// CAFile is a PEM certificate to trust as the relay's root; empty trusts
	// the system roots. Insecure skips verification, for a self-signed dev
	// relay.
	CAFile   string
	Insecure bool
	// SigningKeyFile and Publish, set together, make the publisher sign a fresh
	// credential granting publishing beneath Publish for every session, in
	// place of any ?jwt= in URL, and present a fresh one on the session
	// before its grant lapses. A credential fixed in URL can't be renewed:
	// once it expires the session publishes nothing, so a publisher that runs
	// for longer than its lifetime needs a signing key.
	SigningKeyFile string
	Publish        string
	// QUIC is the QUIC configuration of the session, whether moqt:// or
	// https://; nil is quic-go's defaults.
	QUIC *quic.Config
}

// FromEnv reads the relay the publisher name publishes through from the
// environment, or returns nil when RELAY_URL is unset:
//
//	RELAY_URL          - the relay to publish through, dialed as a client, e.g. "https://relay:4433" or "moqt://relay:4433"; it may carry a credential as ?jwt=
//	RELAY_SIGNING_KEY  - a signing key (qumo auth keygen) to sign a fresh relay credential with on every dial, replacing ?jwt=, and again on the session before its grant lapses
//	RELAY_PUBLISH      - the directory that credential grants publishing beneath, e.g. "/room/"; set with RELAY_SIGNING_KEY
//	RELAY_CA_FILE      - PEM cert to trust as the relay's root (default: the system roots)
//	RELAY_TLS_INSECURE - skip relay TLS verification, for a self-signed dev relay (default: "false")
func FromEnv(name string, quicConfig *quic.Config) (*Upstream, error) {
	relayURL := envconfig.String("RELAY_URL", "")
	if relayURL == "" {
		return nil, nil
	}
	return newUpstream(Config{
		Name:           name,
		URL:            relayURL,
		CAFile:         envconfig.String("RELAY_CA_FILE", ""),
		Insecure:       envconfig.String("RELAY_TLS_INSECURE", "false") == "true",
		SigningKeyFile: envconfig.String("RELAY_SIGNING_KEY", ""),
		Publish:        envconfig.String("RELAY_PUBLISH", ""),
		QUIC:           quicConfig,
	})
}

// Publish keeps a session to the relay open until ctx ends, dialing again
// whenever one ends, so the relay can subscribe to the broadcasts published
// on mux. It returns early only when cfg is unusable.
func Publish(ctx context.Context, cfg Config, mux *moqt.TrackMux) error {
	u, err := newUpstream(cfg)
	if err != nil {
		return err
	}
	u.Run(ctx, mux)
	return nil
}

// newUpstream checks cfg and loads what the sessions need.
func newUpstream(cfg Config) (*Upstream, error) {
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
	u := &Upstream{name: cfg.Name, url: cfg.URL, tls: tc, quic: cfg.QUIC}
	switch {
	case cfg.SigningKeyFile == "" && cfg.Publish == "":
	case cfg.SigningKeyFile == "" || cfg.Publish == "":
		return nil, errors.New("RELAY_SIGNING_KEY and RELAY_PUBLISH must be set together")
	default:
		key, err := token.LoadSigningKey(cfg.SigningKeyFile)
		if err != nil {
			return nil, fmt.Errorf("RELAY_SIGNING_KEY: %w", err)
		}
		publish, err := token.Directory(cfg.Publish)
		if err != nil {
			return nil, fmt.Errorf("RELAY_PUBLISH: %w", err)
		}
		// Signing one now refuses a grant outside the key's prefix at startup
		// rather than at the first dial.
		if _, err := token.Sign(key, publishGrant(publish), token.MaxLifetime); err != nil {
			return nil, fmt.Errorf("RELAY_PUBLISH: %w", err)
		}
		u.key, u.publish = &key, publish
	}
	return u, nil
}

// PublishBeneath returns the directory the publisher's own relay credential
// grants publishing beneath, such as "/acme/comments/", or "" when it signs
// none (no RELAY_SIGNING_KEY) or publishes through no relay (a nil u).
func (u *Upstream) PublishBeneath() string {
	if u == nil {
		return ""
	}
	return u.publish
}

// publishGrant grants publishing every track of every broadcast beneath dir,
// a directory, which is its root.
func publishGrant(dir string) token.Grant {
	return token.Grant{Root: dir, Publish: []string{"/"}}
}

// Run keeps a session to the relay open until ctx ends, dialing again after
// each one ends, with a signed credential fresh on every dial and renewed on
// the session before its grant lapses.
func (u *Upstream) Run(ctx context.Context, mux *moqt.TrackMux) {
	for {
		sess, lapses, err := u.dial(ctx, mux)
		switch {
		case err == nil:
			slog.Info("publishing through the relay", "publisher", u.name)
			if u.key != nil {
				go relaycred.Keep(sess, *u.key, publishGrant(u.publish), lapses)
			}
			select {
			case <-ctx.Done():
				// not actionable: the publisher is stopping.
				_ = sess.CloseWithError(moqt.NoError, u.name+" stopping")
				return
			case <-sess.Context().Done():
				slog.Warn("the relay session ended, dialing again", "publisher", u.name, "cause", moqt.Cause(sess.Context()))
			}
		case ctx.Err() == nil:
			slog.Warn("cannot reach the relay, retrying", "publisher", u.name, "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(retryInterval):
		}
	}
}

// dial opens one session to the relay, serving mux's broadcasts on it, and
// returns when the grant of the credential it signed lapses (zero for none).
func (u *Upstream) dial(ctx context.Context, mux *moqt.TrackMux) (*moqt.Session, time.Time, error) {
	target, lapses, err := u.target()
	if err != nil {
		return nil, time.Time{}, err
	}
	sess, err := (&moqt.Dialer{TLSConfig: u.tls.Clone(), QUICConfig: u.quic}).Dial(ctx, target, mux)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("dial relay: %w", err)
	}
	return sess, lapses, nil
}

// target returns the URL to dial, with a freshly signed credential when the
// upstream signs its own, and when that credential's grant lapses.
func (u *Upstream) target() (string, time.Time, error) {
	if u.key == nil {
		return u.url, time.Time{}, nil
	}
	credential, lapses, err := relaycred.Sign(*u.key, publishGrant(u.publish))
	if err != nil {
		return "", time.Time{}, err
	}
	parsed, err := url.Parse(u.url)
	if err != nil {
		return "", time.Time{}, err
	}
	query := parsed.Query()
	query.Set("jwt", credential)
	parsed.RawQuery = query.Encode()
	return parsed.String(), lapses, nil
}

// RelayName describes relayURL for a startup banner without its credential:
// "none" for no relay.
func RelayName(relayURL string) string {
	if relayURL == "" {
		return "none"
	}
	u, err := url.Parse(relayURL)
	if err != nil {
		return "invalid"
	}
	return u.Scheme + "://" + u.Host + u.Path
}
