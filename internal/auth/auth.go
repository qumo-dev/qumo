// Package auth admits relay sessions (qumo-deploy ADR 0035, Decision 3). A
// client session is admitted by an auth server, asked once per session event,
// or by a static public grant. The relay forwards what it knows about the
// session and enforces the grant it gets back. It never parses a credential:
// keys, projects and quotas are the auth server's business.
//
// The request and grant are a subset of moq-auth's (kixelated/moq), which is
// still changing upstream: subtree patterns only, and no root rewriting,
// mounts or peer flag.
package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	// timeout bounds one request to the auth server.
	timeout = 5 * time.Second
	// maxBody bounds the grant a reply may carry.
	maxBody = 64 << 10

	EventConnect = "connect"

	TransportWebTransport = "webtransport"
	TransportQUIC         = "quic"
)

var (
	errBothSettings = errors.New("QUMO_AUTH_URL and QUMO_AUTH_PUBLIC are both set: " +
		"a relay admits sessions through an auth server or a static public grant, not both")
	errNoSetting = errors.New("neither QUMO_AUTH_URL nor QUMO_AUTH_PUBLIC is set: " +
		"set QUMO_AUTH_URL to an auth server, or QUMO_AUTH_PUBLIC to the patterns anonymous sessions may use " +
		"(QUMO_AUTH_PUBLIC='**' opens everything, for development only)")

	// ErrInvalidGrant is a 2xx reply the relay cannot enforce as given.
	ErrInvalidGrant = errors.New("auth: invalid grant")
	// ErrCredentialOnPublic refuses a session that presents a credential to a
	// relay that verifies none, rather than admitting it on the public grant.
	ErrCredentialOnPublic = errors.New("auth: a credential was presented, but this relay has only a public grant")
)

// RefusedError is an explicit refusal: a 401 or 403 from the auth server, or
// a grant that names nothing.
type RefusedError struct {
	Status int
}

func (e RefusedError) Error() string {
	return fmt.Sprintf("auth: refused (%d)", e.Status)
}

// Client admits sessions. Exactly one of endpoint and public is set.
type Client struct {
	endpoint *url.URL
	client   *http.Client
	public   *Grant
}

// FromEnv reads the configuration from the environment:
//
//	QUMO_AUTH_URL    - the auth server; https, or http on a loopback host
//	QUMO_AUTH_PUBLIC - comma-separated subtree patterns ("anon/**") that any
//	                   session may publish and subscribe to; no server
//
// Exactly one must be set.
func FromEnv() (*Client, error) {
	rawURL := os.Getenv("QUMO_AUTH_URL")
	public := os.Getenv("QUMO_AUTH_PUBLIC")
	switch {
	case rawURL != "" && public != "":
		return nil, errBothSettings
	case rawURL != "":
		c, err := New(rawURL)
		if err != nil {
			return nil, fmt.Errorf("QUMO_AUTH_URL: %w", err)
		}
		return c, nil
	case public != "":
		c, err := NewPublic(public)
		if err != nil {
			return nil, fmt.Errorf("QUMO_AUTH_PUBLIC: %w", err)
		}
		return c, nil
	default:
		return nil, errNoSetting
	}
}

// New returns a Client that asks the auth server at rawURL: https, or http
// only on a loopback host, since the request carries the client's credential.
func New(rawURL string) (*Client, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	switch u.Scheme {
	case "https":
	case "http":
		host := u.Hostname()
		if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return nil, fmt.Errorf("http is allowed only for a loopback host, got %q", host)
		}
	default:
		return nil, fmt.Errorf("want an https URL, got scheme %q", u.Scheme)
	}
	return &Client{endpoint: u, client: &http.Client{Timeout: timeout}}, nil
}

// NewPublic returns a Client that admits every session on a static grant: the
// comma-separated subtree patterns, to both publish and subscribe.
func NewPublic(patterns string) (*Client, error) {
	p, err := parsePatterns(strings.Split(patterns, ","))
	if err != nil {
		return nil, err
	}
	return &Client{public: &Grant{publish: p, subscribe: p}}, nil
}

// String names the admission mode for the startup log.
func (c *Client) String() string {
	if c.public != nil {
		return "public grant"
	}
	return c.endpoint.String()
}

// Request is one session event sent to the auth server.
type Request struct {
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

// Connect asks whether a new session may start, and returns its grant. The
// error is a RefusedError for an explicit refusal; any other error means the
// auth server could not answer, which refuses too.
func (c *Client) Connect(ctx context.Context, req Request) (*Grant, error) {
	if c.public != nil {
		if q, err := url.ParseQuery(req.Query); err == nil && q.Has("jwt") {
			return nil, ErrCredentialOnPublic
		}
		return c.public, nil
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("auth: encode request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("auth: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := c.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("auth: %w", err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBody))
		return nil, RefusedError{Status: resp.StatusCode}
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBody))
		return nil, fmt.Errorf("auth: server answered %d", resp.StatusCode)
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return nil, fmt.Errorf("auth: read grant: %w", err)
	}
	if len(raw) > maxBody {
		return nil, fmt.Errorf("%w: larger than %d bytes", ErrInvalidGrant, maxBody)
	}
	return parseGrant(raw, time.Now())
}

// RefusalStatus is the HTTP status a WebTransport client gets for err.
func RefusalStatus(err error) int {
	var refused RefusedError
	switch {
	case errors.As(err, &refused):
		return refused.Status
	case errors.Is(err, ErrCredentialOnPublic):
		return http.StatusUnauthorized
	default:
		return http.StatusServiceUnavailable
	}
}
