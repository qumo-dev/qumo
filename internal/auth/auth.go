// Package auth asks an auth server whether a relay session may start
// (qumo-deploy ADR 0035, Decision 3). The relay forwards what it knows about
// the session and enforces the grant it gets back. It never parses a
// credential: keys, projects and quotas are the auth server's business.
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
	"time"
)

const (
	// timeout bounds one request to the auth server.
	timeout = 5 * time.Second
	// maxBody bounds the grant a reply may carry.
	maxBody = 64 << 10
)

// Session events, the Request's Event.
const (
	EventConnect = "connect"
)

// Transports, the Request's Transport.
const (
	TransportWebTransport = "webtransport"
	TransportQUIC         = "quic"
)

// ErrInvalidGrant is a 2xx reply the relay cannot enforce as given.
var ErrInvalidGrant = errors.New("auth: invalid grant")

// RefusedError is an explicit refusal: a 401 or 403 from the auth server, or
// a grant that names nothing.
type RefusedError struct {
	Status int
}

func (e RefusedError) Error() string {
	return fmt.Sprintf("auth: refused (%d)", e.Status)
}

// RefusalStatus is the HTTP status a WebTransport client gets for err: the
// refusal's status, or 503 when the auth server could not answer.
func RefusalStatus(err error) int {
	if refused, ok := errors.AsType[RefusedError](err); ok {
		return refused.Status
	}
	return http.StatusServiceUnavailable
}

// Config is the relay's auth setting.
type Config struct {
	// URL is the auth server asked about every session.
	URL string
}

// LoadConfig reads QUMO_AUTH_URL, the auth server: https, or http on a
// loopback host. It is required: a relay admits every client session through
// its auth server.
func LoadConfig() (Config, error) {
	rawURL := os.Getenv("QUMO_AUTH_URL")
	if rawURL == "" {
		return Config{}, errors.New("QUMO_AUTH_URL is not set: point it at the auth server beside this relay " +
			"(qumo auth; QUMO_AUTH_ANONYMOUS='**' there opens everything, for development only)")
	}
	return Config{URL: rawURL}, nil
}

// Client asks an auth server about sessions.
type Client struct {
	endpoint *url.URL
	client   *http.Client
}

// NewClient returns a Client for the auth server at rawURL: https, or http
// only on a loopback host, since the request carries the client's credential.
func NewClient(rawURL string) (*Client, error) {
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
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBody)) // not actionable: drained to reuse the connection
		return nil, RefusedError{Status: resp.StatusCode}
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBody)) // not actionable: drained to reuse the connection
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
