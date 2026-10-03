package relay

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/url"

	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/qumo-dev/qumo/internal/auth"
)

// grantKey carries a WebTransport session's grant from the upgrade request
// into the session, whose context derives from the request's.
type grantKey struct{}

// newSessionID returns a random 128-bit hex id, unique per session.
func newSessionID() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never fails
	return hex.EncodeToString(b[:])
}

// webTransportRequest describes a WebTransport upgrade for the auth server.
func (s *Server) webTransportRequest(r *http.Request) auth.Request {
	req := auth.Request{
		ID:        newSessionID(),
		Event:     auth.EventConnect,
		Node:      s.nodeID(),
		Transport: auth.TransportWebTransport,
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
// and query come from the SETUP Path parameter (gomoqt's Session.RequestURI),
// decoded the same way a WebTransport request's are.
func (s *Server) nativeRequest(sess *moqt.Session) auth.Request {
	req := auth.Request{
		ID:        newSessionID(),
		Event:     auth.EventConnect,
		Node:      s.nodeID(),
		Transport: auth.TransportQUIC,
		Path:      sess.RequestURI(),
	}
	if u, err := url.ParseRequestURI(sess.RequestURI()); err == nil {
		req.Path, req.Query = u.Path, u.RawQuery
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
func (s *Server) admit(ctx context.Context, req auth.Request) (*auth.Grant, error) {
	g, err := s.auth.Connect(ctx, req)
	var refused auth.RefusedError
	switch {
	case err == nil:
		metricAuthRequests.WithLabelValues(auth.EventConnect, "admitted").Inc()
	case errors.As(err, &refused), errors.Is(err, auth.ErrCredentialOnPublic):
		metricAuthRequests.WithLabelValues(auth.EventConnect, "refused").Inc()
		slog.Info("relay: session refused", "transport", req.Transport, "remote", req.Remote,
			"path", req.Path, "reason", err)
	case errors.Is(err, auth.ErrInvalidGrant):
		metricAuthRequests.WithLabelValues(auth.EventConnect, "invalid").Inc()
		slog.Error("relay: session refused: the auth server's grant can't be enforced", "transport", req.Transport,
			"remote", req.Remote, "path", req.Path, "error", err)
	default:
		metricAuthRequests.WithLabelValues(auth.EventConnect, "error").Inc()
		slog.Error("relay: session refused: auth server unavailable", "transport", req.Transport,
			"remote", req.Remote, "path", req.Path, "error", err)
	}
	return g, err
}
