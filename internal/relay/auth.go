package relay

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/qumo-dev/gomoqt/moqt"
)

// Session admission (qumo-deploy ADR 0035, Decision 3). A client session is
// admitted by an auth server, asked once per session event, or by a static
// public grant. The relay forwards what it knows about the session and
// enforces the grant it gets back. It never parses a credential: keys,
// projects and quotas are the auth server's business.
//
// The request and grant are a subset of moq-auth's (kixelated/moq), which is
// still changing upstream: subtree patterns only, and no root rewriting,
// mounts or peer flag.

const (
	// authTimeout bounds one request to the auth server.
	authTimeout = 5 * time.Second
	// authMaxBody bounds the grant a reply may carry.
	authMaxBody = 64 << 10

	eventConnect = "connect"

	transportWebTransport = "webtransport"
	transportQUIC         = "quic"
)

var (
	errBothAuthSettings = errors.New("QUMO_AUTH_URL and QUMO_AUTH_PUBLIC are both set: " +
		"a relay admits sessions through an auth server or a static public grant, not both")
	errNoAuthSetting = errors.New("neither QUMO_AUTH_URL nor QUMO_AUTH_PUBLIC is set: " +
		"set QUMO_AUTH_URL to an auth server, or QUMO_AUTH_PUBLIC to the patterns anonymous sessions may use " +
		"(QUMO_AUTH_PUBLIC='**' opens everything, for development only)")

	// errInvalidGrant is a 2xx reply the relay cannot enforce as given.
	errInvalidGrant = errors.New("auth: invalid grant")
	// errCredentialOnPublic refuses a session that presents a credential to a
	// relay that verifies none, rather than admitting it on the public grant.
	errCredentialOnPublic = errors.New("auth: a credential was presented, but this relay has only a public grant")
)

// refusedError is an explicit refusal: a 401 or 403 from the auth server, or
// a grant that names nothing.
type refusedError struct {
	status int
}

func (e refusedError) Error() string {
	return fmt.Sprintf("auth: refused (%d)", e.status)
}

// sessionAuth admits client sessions. Exactly one of endpoint and public is
// set.
type sessionAuth struct {
	endpoint *url.URL
	client   *http.Client
	public   *grant
}

// newSessionAuth reads the configuration from the environment:
//
//	QUMO_AUTH_URL    - the auth server; https, or http on a loopback host
//	QUMO_AUTH_PUBLIC - comma-separated subtree patterns ("anon/**") that any
//	                   session may publish and subscribe to; no server
//
// Exactly one must be set.
func newSessionAuth() (*sessionAuth, error) {
	rawURL := os.Getenv("QUMO_AUTH_URL")
	public := os.Getenv("QUMO_AUTH_PUBLIC")
	switch {
	case rawURL != "" && public != "":
		return nil, errBothAuthSettings
	case rawURL != "":
		endpoint, err := parseAuthURL(rawURL)
		if err != nil {
			return nil, fmt.Errorf("QUMO_AUTH_URL: %w", err)
		}
		return &sessionAuth{endpoint: endpoint, client: &http.Client{Timeout: authTimeout}}, nil
	case public != "":
		patterns, err := parsePatterns(strings.Split(public, ","))
		if err != nil {
			return nil, fmt.Errorf("QUMO_AUTH_PUBLIC: %w", err)
		}
		return &sessionAuth{public: &grant{publish: patterns, subscribe: patterns}}, nil
	default:
		return nil, errNoAuthSetting
	}
}

// parseAuthURL accepts https, and http only on a loopback host: the request
// carries the client's credential.
func parseAuthURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	switch u.Scheme {
	case "https":
		return u, nil
	case "http":
		host := u.Hostname()
		if ip := net.ParseIP(host); host == "localhost" || (ip != nil && ip.IsLoopback()) {
			return u, nil
		}
		return nil, fmt.Errorf("http is allowed only for a loopback host, got %q", host)
	default:
		return nil, fmt.Errorf("want an https URL, got scheme %q", u.Scheme)
	}
}

// describe names the admission mode for the startup log.
func (a *sessionAuth) describe() string {
	if a.public != nil {
		return "public grant"
	}
	return a.endpoint.String()
}

// authRequest is one session event sent to the auth server.
type authRequest struct {
	ID         string `json:"id"`
	Event      string `json:"event"`
	Node       string `json:"node,omitempty"`
	Transport  string `json:"transport"`
	Remote     string `json:"remote,omitempty"`
	Local      string `json:"local,omitempty"`
	ServerName string `json:"server_name,omitempty"`
	Path       string `json:"path"`
	Query      string `json:"query,omitempty"`
}

// newSessionID returns a random 128-bit hex id, unique per session.
func newSessionID() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never fails
	return hex.EncodeToString(b[:])
}

// connect asks whether a new session may start, and returns its grant. The
// error is a refusedError for an explicit refusal; any other error means the
// auth server could not answer, which refuses too.
func (a *sessionAuth) connect(ctx context.Context, req authRequest) (*grant, error) {
	if a.public != nil {
		if q, err := url.ParseQuery(req.Query); err == nil && q.Has("jwt") {
			return nil, errCredentialOnPublic
		}
		return a.public, nil
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("auth: encode request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, a.endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("auth: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := a.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("auth: %w", err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, authMaxBody))
		return nil, refusedError{status: resp.StatusCode}
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, authMaxBody))
		return nil, fmt.Errorf("auth: server answered %d", resp.StatusCode)
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, authMaxBody+1))
	if err != nil {
		return nil, fmt.Errorf("auth: read grant: %w", err)
	}
	if len(raw) > authMaxBody {
		return nil, fmt.Errorf("%w: larger than %d bytes", errInvalidGrant, authMaxBody)
	}
	return parseGrant(raw, time.Now())
}

// refusalStatus is the HTTP status a WebTransport client gets for err.
func refusalStatus(err error) int {
	var refused refusedError
	switch {
	case errors.As(err, &refused):
		return refused.status
	case errors.Is(err, errCredentialOnPublic):
		return http.StatusUnauthorized
	default:
		return http.StatusServiceUnavailable
	}
}

// grant is what a session may do.
type grant struct {
	publish   []pattern
	subscribe []pattern
	// expires is when the session must end; zero means never (public grant).
	expires time.Time
	// revalidate is how often to ask again; zero means never.
	revalidate time.Duration
}

// grantWire is the grant as the auth server sends it. root, mounts and peer
// are read only to refuse a grant that uses them.
type grantWire struct {
	Publish    []string        `json:"publish"`
	Subscribe  []string        `json:"subscribe"`
	Expires    *int64          `json:"expires"`
	Revalidate *int64          `json:"revalidate"`
	Root       string          `json:"root"`
	Mounts     json.RawMessage `json:"mounts"`
	Peer       bool            `json:"peer"`
}

func parseGrant(raw []byte, now time.Time) (*grant, error) {
	var w grantWire
	if err := json.Unmarshal(raw, &w); err != nil {
		return nil, fmt.Errorf("%w: %w", errInvalidGrant, err)
	}
	if w.Root != "" || (len(w.Mounts) > 0 && string(w.Mounts) != "null" && string(w.Mounts) != "{}") || w.Peer {
		return nil, fmt.Errorf("%w: root, mounts and peer are not supported", errInvalidGrant)
	}
	publish, err := parsePatterns(w.Publish)
	if err != nil {
		return nil, fmt.Errorf("%w: publish: %w", errInvalidGrant, err)
	}
	subscribe, err := parsePatterns(w.Subscribe)
	if err != nil {
		return nil, fmt.Errorf("%w: subscribe: %w", errInvalidGrant, err)
	}
	if len(publish) == 0 && len(subscribe) == 0 {
		return nil, refusedError{status: http.StatusForbidden}
	}

	g := &grant{publish: publish, subscribe: subscribe}
	if w.Expires != nil {
		g.expires = time.Unix(*w.Expires, 0)
		if !g.expires.After(now) {
			return nil, fmt.Errorf("%w: expires is not in the future", errInvalidGrant)
		}
	}
	if w.Revalidate != nil {
		if *w.Revalidate <= 0 || w.Expires == nil {
			return nil, fmt.Errorf("%w: revalidate needs a positive value and an expires", errInvalidGrant)
		}
		g.revalidate = time.Duration(*w.Revalidate) * time.Second
	}
	return g, nil
}

// mayPublish reports whether the grant covers announcing path.
func (g *grant) mayPublish(path moqt.BroadcastPath) bool {
	return anyCovers(g.publish, path)
}

func anyCovers(patterns []pattern, path moqt.BroadcastPath) bool {
	for _, p := range patterns {
		if p.covers(path) {
			return true
		}
	}
	return false
}

// pattern is a subtree pattern: "**" (everything) or "a/b/**" (a/b and
// everything beneath it, on "/" boundaries). base is "" for "**".
type pattern struct {
	base string
}

func parsePatterns(raw []string) ([]pattern, error) {
	patterns := make([]pattern, 0, len(raw))
	for _, s := range raw {
		p, err := parsePattern(strings.TrimSpace(s))
		if err != nil {
			return nil, err
		}
		patterns = append(patterns, p)
	}
	return patterns, nil
}

func parsePattern(s string) (pattern, error) {
	if s == "**" {
		return pattern{}, nil
	}
	base, ok := strings.CutSuffix(s, "/**")
	if !ok {
		return pattern{}, fmt.Errorf("pattern %q: only subtree patterns (\"**\", \"a/b/**\") are supported", s)
	}
	for seg := range strings.SplitSeq(base, "/") {
		if seg == "" || seg == "." || seg == ".." || strings.Contains(seg, "*") {
			return pattern{}, fmt.Errorf("pattern %q: bad segment %q", s, seg)
		}
	}
	return pattern{base: base}, nil
}

// covers reports whether path lies at or beneath the pattern's base. A
// broadcast path is rooted at "/"; patterns are relative to it.
func (p pattern) covers(path moqt.BroadcastPath) bool {
	if p.base == "" {
		return true
	}
	rel := strings.TrimPrefix(path.String(), "/")
	return rel == p.base || strings.HasPrefix(rel, p.base+"/")
}

// grantKey carries a WebTransport session's grant from the upgrade request
// into the session, whose context derives from the request's.
type grantKey struct{}

// webTransportRequest describes a WebTransport upgrade for the auth server.
func (s *Server) webTransportRequest(r *http.Request) authRequest {
	req := authRequest{
		ID:        newSessionID(),
		Event:     eventConnect,
		Node:      s.nodeID(),
		Transport: transportWebTransport,
		Remote:    r.RemoteAddr,
		Path:      r.URL.Path,
		Query:     r.URL.RawQuery,
	}
	if local, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr); ok {
		req.Local = local.String()
	}
	if r.TLS != nil {
		req.ServerName = r.TLS.ServerName
	}
	return req
}

// nativeRequest describes a native-QUIC session for the auth server. Its path
// comes from the SETUP Path parameter; a client that put a query there gets
// it forwarded as the query.
func (s *Server) nativeRequest(sess *moqt.Session) authRequest {
	path, query, _ := strings.Cut(sess.RequestPath(), "?")
	req := authRequest{
		ID:        newSessionID(),
		Event:     eventConnect,
		Node:      s.nodeID(),
		Transport: transportQUIC,
		Path:      path,
		Query:     query,
	}
	if addr := sess.RemoteAddr(); addr != nil {
		req.Remote = addr.String()
	}
	if addr := sess.LocalAddr(); addr != nil {
		req.Local = addr.String()
	}
	if state := sess.ConnectionState().TLS; state != nil {
		req.ServerName = state.ServerName
	}
	return req
}

func (s *Server) nodeID() string {
	if s.Config == nil {
		return ""
	}
	return s.Config.NodeID
}

// admit runs the connect check and records its outcome.
func (s *Server) admit(ctx context.Context, req authRequest) (*grant, error) {
	g, err := s.auth.connect(ctx, req)
	var refused refusedError
	switch {
	case err == nil:
		metricAuthRequests.WithLabelValues(eventConnect, "admitted").Inc()
	case errors.As(err, &refused), errors.Is(err, errCredentialOnPublic):
		metricAuthRequests.WithLabelValues(eventConnect, "refused").Inc()
		slog.Info("relay: session refused", "transport", req.Transport, "remote", req.Remote,
			"path", req.Path, "reason", err)
	case errors.Is(err, errInvalidGrant):
		metricAuthRequests.WithLabelValues(eventConnect, "invalid").Inc()
		slog.Error("relay: session refused: the auth server's grant can't be enforced", "transport", req.Transport,
			"remote", req.Remote, "path", req.Path, "error", err)
	default:
		metricAuthRequests.WithLabelValues(eventConnect, "error").Inc()
		slog.Error("relay: session refused: auth server unavailable", "transport", req.Transport,
			"remote", req.Remote, "path", req.Path, "error", err)
	}
	return g, err
}
