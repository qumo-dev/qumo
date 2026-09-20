package rtsp

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Client is an RTSP client connected to a server (e.g. an IP camera). It speaks
// the client side of DESCRIBE/SETUP/PLAY over a single TCP-interleaved
// connection, leaving the interleaved-RTP read loop to the caller.
type Client struct {
	conn           *Conn
	url            *url.URL
	cred           Credentials
	cseq           int
	sessionID      string        // sent on SETUP/PLAY/TEARDOWN after the first SETUP.
	sessionTimeout time.Duration // parsed from Session header; 0 means not specified.
}

// Dial connects to the RTSP server at rawURL (which may carry user:pass for
// auth) and returns a ready Client. The caller should [Client.Describe] next.
func Dial(ctx context.Context, rawURL string) (*Client, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("rtsp: parse url: %w", err)
	}
	if !strings.HasPrefix(strings.ToLower(u.Scheme), "rtsp") {
		return nil, fmt.Errorf("rtsp: not an rtsp url: %s", rawURL)
	}
	host := u.Host
	if !strings.Contains(host, ":") {
		host += ":554"
	}
	d := net.Dialer{}
	nc, err := d.DialContext(ctx, "tcp", host)
	if err != nil {
		return nil, fmt.Errorf("rtsp: dial %s: %w", host, err)
	}
	var cred Credentials
	if u.User != nil {
		cred.Username = u.User.Username()
		cred.Password, _ = u.User.Password()
	}
	// Strip credentials from the URL so they don't appear in request lines.
	u.User = nil
	return &Client{conn: NewConn(nc), url: u, cred: cred}, nil
}

// Close sends TEARDOWN (best-effort) and closes the underlying connection.
func (c *Client) Close() error {
	if c.sessionID != "" {
		req := c.newRequest(MethodTeardown, c.url.String())
		_ = c.conn.WriteRequest(req) // best-effort; ignore response/errors.
	}
	return c.conn.Close()
}

// Describe sends DESCRIBE and returns the parsed SDP.
func (c *Client) Describe(ctx context.Context) (*SDP, error) {
	req := c.newRequest(MethodDescribe, c.url.String())
	req.Header.Set("Accept", "application/sdp")
	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != StatusOK {
		return nil, fmt.Errorf("rtsp: DESCRIBE %s", resp.Status)
	}
	if resp.Body == nil {
		return nil, fmt.Errorf("rtsp: DESCRIBE returned no body")
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("rtsp: read DESCRIBE body: %w", err)
	}
	return ParseSDP(string(body)), nil
}

// SetupChannel is the server-assigned interleaved channel pair for one track.
type SetupChannel struct {
	RTP  uint8
	RTCP uint8
}

// Setup sends SETUP for one track, requesting interleaved TCP transport on the
// supplied channel pair (the server may accept or reassign them). The returned
// SetupChannel is what the server actually assigned (parsed from its Transport
// response header).
func (c *Client) Setup(ctx context.Context, trackURL string, rtpCh, rtcpCh uint8) (SetupChannel, error) {
	transport := fmt.Sprintf("RTP/AVP/TCP;unicast;interleaved=%d-%d", rtpCh, rtcpCh)
	req := c.newRequest(MethodSetup, trackURL)
	req.Header.Set("Transport", transport)
	if c.sessionID != "" {
		req.Header.Set("Session", c.sessionID)
	}
	resp, err := c.do(req)
	if err != nil {
		return SetupChannel{}, err
	}
	if resp.StatusCode != StatusOK {
		return SetupChannel{}, fmt.Errorf("rtsp: SETUP %s for %s", resp.Status, trackURL)
	}
	// Capture the session ID and optional timeout for subsequent requests.
	if sid := resp.Header.Get("Session"); sid != "" {
		// Session IDs may carry a timeout suffix: "12345;timeout=60".
		c.sessionID = strings.Split(sid, ";")[0]
		c.sessionTimeout = parseSessionTimeout(sid)
	}
	// Parse the server-assigned channels from its Transport response.
	gotRTP, gotRTCP, ok := parseInterleaved(resp.Header.Get("Transport"))
	if !ok {
		// Server didn't echo interleaved; trust our request.
		gotRTP, gotRTCP = rtpCh, rtcpCh
	}
	return SetupChannel{RTP: gotRTP, RTCP: gotRTCP}, nil
}

// Play sends PLAY on the aggregate session URL to begin streaming.
func (c *Client) Play(ctx context.Context) error {
	req := c.newRequest(MethodPlay, c.url.String())
	if c.sessionID != "" {
		req.Header.Set("Session", c.sessionID)
	}
	resp, err := c.do(req)
	if err != nil {
		return err
	}
	if resp.StatusCode != StatusOK {
		return fmt.Errorf("rtsp: PLAY %s", resp.Status)
	}
	return nil
}

// ReadInterleaved blocks until the next interleaved frame (RTP or RTCP) arrives
// on the connection. It returns io.EOF/err when the connection closes.
func (c *Client) ReadInterleaved() (*InterleavedFrame, error) {
	for {
		_, frame, err := c.conn.ReadRequest()
		if err != nil {
			return nil, err
		}
		if frame != nil {
			return frame, nil
		}
		// A non-interleaved RTSP request arrived on the control channel — RTSP
		// servers rarely send server-initiated requests; ignore and keep reading.
	}
}

// SessionTimeout returns the session timeout advertised by the server in the
// Session header (e.g. "12345;timeout=60"). Returns 0 if the server did not
// specify a timeout or SETUP has not been called yet.
func (c *Client) SessionTimeout() time.Duration {
	return c.sessionTimeout
}

// SendKeepalive sends a GET_PARAMETER request to keep the RTSP session alive.
// The server's 200 OK response is harmlessly consumed by [Client.ReadInterleaved]
// (parsed as a non-interleaved message and discarded), so it is safe to call
// from a goroutine concurrent with ReadInterleaved.
//
// Callers should send keepalives at an interval shorter than [Client.SessionTimeout]
// (typically timeout/2) to prevent the server from tearing down the session.
func (c *Client) SendKeepalive() error {
	req := c.newRequest(MethodGetParameter, c.url.String())
	if c.sessionID != "" {
		req.Header.Set("Session", c.sessionID)
	}
	return c.conn.WriteRequest(req)
}

// --- internal helpers ---

func (c *Client) newRequest(method Method, uri string) *Request {
	c.cseq++
	req := &Request{
		Method: method,
		URL:    mustParseURL(uri),
		Proto:  "RTSP/1.0",
		Header: make(http.Header),
	}
	req.Header.Set("CSeq", strconv.Itoa(c.cseq))
	req.Header.Set("User-Agent", "qumo-rtsp-client")
	return req
}

// do sends a request and handles 401 auth retry. If the server responds 401 and
// the URL carried credentials, it parses WWW-Authenticate, builds Authorization,
// and resends the request once.
func (c *Client) do(req *Request) (*Response, error) {
	resp, err := c.roundtrip(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == StatusUnauthorized && c.cred.HasCredentials() {
		ch, ok := ParseAuthChallenge(resp.Header.Get("WWW-Authenticate"))
		if !ok {
			return resp, fmt.Errorf("rtsp: 401 with no parseable WWW-Authenticate")
		}
		authVal, err := BuildAuthorization(string(req.Method), req.URL.String(), c.cred, ch)
		if err != nil {
			return resp, fmt.Errorf("rtsp: build auth: %w", err)
		}
		req.Header.Set("Authorization", authVal)
		// Resend with a new CSeq.
		c.cseq++
		req.Header.Set("CSeq", strconv.Itoa(c.cseq))
		return c.roundtrip(req)
	}
	return resp, nil
}

func (c *Client) roundtrip(req *Request) (*Response, error) {
	if err := c.conn.WriteRequest(req); err != nil {
		return nil, fmt.Errorf("rtsp: write %s: %w", req.Method, err)
	}
	resp, _, err := c.conn.ReadResponse(req)
	if err != nil {
		return nil, fmt.Errorf("rtsp: read %s response: %w", req.Method, err)
	}
	return resp, nil
}

// parseInterleaved extracts the interleaved=X-Y channel pair from a Transport
// header (the SETUP response side).
func parseInterleaved(transport string) (rtp, rtcp uint8, ok bool) {
	const token = "interleaved="
	_, after, ok := strings.Cut(transport, token)
	if !ok {
		return 0, 0, false
	}
	rest := after
	var a, b int
	if n, err := fmt.Sscanf(rest, "%d-%d", &a, &b); err == nil && n == 2 {
		return uint8(a), uint8(b), true
	}
	if n, err := fmt.Sscanf(rest, "%d", &a); err == nil && n == 1 {
		return uint8(a), uint8(a), true
	}
	return 0, 0, false
}

func mustParseURL(s string) *url.URL {
	u, err := url.Parse(s)
	if err != nil {
		panic(err) // URLs are constructed internally; should never fail.
	}
	return u
}

// parseSessionTimeout extracts the timeout value from an RTSP Session header.
// The header format is "session-id[;timeout=seconds]" (RFC 2326 §12.37).
// Returns 0 if no timeout parameter is present.
func parseSessionTimeout(session string) time.Duration {
	for _, part := range strings.Split(session, ";") {
		part = strings.TrimSpace(part)
		key, val, ok := strings.Cut(part, "=")
		if ok && strings.TrimSpace(strings.ToLower(key)) == "timeout" {
			if sec, err := strconv.Atoi(strings.TrimSpace(val)); err == nil && sec > 0 {
				return time.Duration(sec) * time.Second
			}
		}
	}
	return 0
}
