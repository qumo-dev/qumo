package auth

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/qumo-dev/qumo/token"
)

// maxRequestBytes bounds a relay's session-event body.
const maxRequestBytes = 64 << 10

// grantResponse is a Grant as the auth server writes it: subtree patterns,
// and when the session ends.
type grantResponse struct {
	Publish   []string `json:"publish,omitempty"`
	Subscribe []string `json:"subscribe,omitempty"`
	// Expires is when the relay ends the session, in unix seconds: the
	// token's exp plus the leeway it was accepted within. Omitted for an
	// anonymous session, which has no token to expire. No revalidate is
	// sent: static keys can't change the answer before then.
	Expires int64 `json:"expires,omitzero"`
}

// Handler answers a relay's session events: connect and revalidate get a
// grant or a refusal, end is acknowledged.
type Handler struct {
	// Keys are the trusted signing keys, by kid.
	Keys map[string]token.Key
	// Anonymous is the grant for a session that presents no credential: the
	// subtree patterns ("anon/**", or "**" in development) it may publish and
	// subscribe to. Empty refuses such sessions. A session that presents a
	// credential is always verified, never granted Anonymous instead.
	Anonymous []string
}

// ServeHTTP answers one session event POSTed by a relay.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeError(w, http.StatusMethodNotAllowed, "POST a session event")
		return
	}
	var req Request
	if err := json.UnmarshalRead(http.MaxBytesReader(w, r.Body, maxRequestBytes), &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid session event")
		return
	}

	switch req.Event {
	case EventConnect, EventRevalidate:
		h.answer(w, req)
	case EventEnd:
		slog.Info("auth server: session ended", "id", req.ID, "reason", req.Reason, "duration_s", req.Duration,
			"bytes_sent", req.Bytes.Sent, "bytes_received", req.Bytes.Received)
		w.WriteHeader(http.StatusNoContent)
	default:
		writeError(w, http.StatusBadRequest, "unknown event")
	}
}

// answer verifies the token in the request's query and writes its grant, or
// a refusal: 401 for a token that can't be accepted, 403 for a valid one that
// grants nothing usable.
func (h *Handler) answer(w http.ResponseWriter, req Request) {
	g, err := h.grantFor(req)
	if err != nil {
		status := http.StatusUnauthorized
		if errors.Is(err, token.ErrForbidden) {
			status = http.StatusForbidden
		}
		// The query carries the credential; it is never logged.
		slog.Info("auth server: session refused", "id", req.ID, "event", req.Event,
			"transport", req.Transport, "remote", req.Remote, "path", req.Path, "reason", err)
		writeError(w, status, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.MarshalWrite(w, g); err != nil {
		slog.Warn("auth server: write grant", "id", req.ID, "error", err)
	}
}

func (h *Handler) grantFor(req Request) (grantResponse, error) {
	query, err := url.ParseQuery(req.Query)
	if err != nil {
		return grantResponse{}, fmt.Errorf("%w: query: %v", token.ErrInvalid, err)
	}
	if !query.Has("jwt") {
		if len(h.Anonymous) == 0 {
			return grantResponse{}, fmt.Errorf("%w: no credential: the client must connect with ?jwt=", token.ErrInvalid)
		}
		return grantResponse{Publish: h.Anonymous, Subscribe: h.Anonymous}, nil
	}
	c, err := token.Verify(query.Get("jwt"), h.Keys, time.Now())
	if err != nil {
		return grantResponse{}, err
	}
	g := grantResponse{Expires: c.ExpiresAt.Add(token.Leeway).Unix()}
	if c.Publish != "" {
		g.Publish = []string{c.Publish + "/**"}
	}
	if c.Subscribe != "" {
		g.Subscribe = []string{c.Subscribe + "/**"}
	}
	return g, nil
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.MarshalWrite(w, map[string]string{"error": msg}) // not actionable: the status is already sent
}
