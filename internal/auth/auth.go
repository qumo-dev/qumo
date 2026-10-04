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
	// endTimeout bounds an end report: it is best effort, sent once.
	endTimeout = 2 * time.Second
	// maxBody bounds the grant a reply may carry.
	maxBody = 64 << 10
)

// Session events, the Request's Event.
const (
	// EventConnect asks whether a new session may start.
	EventConnect = "connect"
	// EventRevalidate asks again, at the grant's revalidate cadence, whether
	// a live session may continue.
	EventRevalidate = "revalidate"
	// EventEnd reports that a session ended, with its final byte totals.
	EventEnd = "end"
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
	// URL is the auth server asked about every session. Empty turns auth
	// off: the relay admits every session unchecked.
	URL string
}

// LoadConfig reads QUMO_AUTH_URL, the auth server: https, or http on a
// loopback host. It is optional; unset, the relay runs with auth off.
func LoadConfig() Config {
	return Config{URL: os.Getenv("QUMO_AUTH_URL")}
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
	return &Client{endpoint: u, client: &http.Client{
		Timeout: timeout,
		// Never follow a redirect: a 307 or 308 would re-POST the client's
		// credential to wherever Location points, past the scheme and
		// loopback checks above. A 3xx is answered like any non-2xx: refused.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
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

	// Bytes is the session's cumulative byte totals, sent on revalidate
	// and end. Reporting them on revalidate as well as end is qumo's one
	// extension of moq-auth, so billing sees a long session before it ends.
	// Zero totals are left out, so an absent bytes means none.
	Bytes Bytes `json:"bytes,omitzero"`
	// Reason is why the session ended, sent on end.
	Reason string `json:"reason,omitempty"`
	// Duration is how long the session lasted in whole seconds, sent on end.
	Duration int64 `json:"duration,omitempty"`
}

// Bytes is a session's byte totals, both directions from the relay's point
// of view.
type Bytes struct {
	// Sent is the bytes the relay sent to the peer.
	Sent uint64 `json:"sent"`
	// Received is the bytes the relay received from the peer.
	Received uint64 `json:"received"`
}

// End reports req, an end event. It is best effort: one attempt, bounded by
// a short timeout, and its reply is not read beyond the status. The error is
// for the caller to record; nothing depends on it.
func (c *Client) End(ctx context.Context, req Request) error {
	ctx, cancel := context.WithTimeout(ctx, endTimeout)
	defer cancel()
	resp, err := c.post(ctx, req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBody)) // not actionable: drained to reuse the connection
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("auth: server answered %d", resp.StatusCode)
	}
	return nil
}

// post sends req to the auth server as JSON.
func (c *Client) post(ctx context.Context, req Request) (*http.Response, error) {
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
	return resp, nil
}

// Authorize sends req, a connect or revalidate event, and returns the grant.
// The error is a RefusedError for an explicit refusal, wraps ErrInvalidGrant
// for a grant the relay cannot enforce, and is otherwise an auth server that
// could not answer.
func (c *Client) Authorize(ctx context.Context, req Request) (*Grant, error) {
	resp, err := c.post(ctx, req)
	if err != nil {
		return nil, err
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
