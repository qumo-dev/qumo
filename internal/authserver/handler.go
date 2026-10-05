// Package authserver is qumo's auth server: it answers a relay's session
// events (QUMO_AUTH_URL on the relay) by verifying the capability token a
// client presented in its connect URL, and turning the token's grant into the
// publish and subscribe patterns the relay enforces. It also holds the
// "qumo auth" command: serve, keygen and token.
package authserver

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/qumo-dev/qumo/internal/auth"
	"github.com/qumo-dev/qumo/token"
)

// maxRequestBytes bounds a relay's session-event body.
const maxRequestBytes = 64 << 10

// grant is what a session may do, as the relay receives it (auth.Grant on the
// relay's side): subtree patterns, and when the session ends.
type grant struct {
	Publish   []string `json:"publish,omitempty"`
	Subscribe []string `json:"subscribe,omitempty"`
	// Expires is when the relay ends the session, in unix seconds: the
	// token's exp plus the leeway it was accepted within. Omitted for an
	// anonymous session, which has no token to expire.
	Expires int64 `json:"expires,omitzero"`
	// Revalidate is how often the relay asks again, in seconds; omitted when
	// nothing could change the answer (static keys).
	Revalidate int64 `json:"revalidate,omitzero"`
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
	// Revalidate is how often a relay should ask again; zero tells it not
	// to (static keys never change).
	Revalidate time.Duration

	now func() time.Time // nil is time.Now
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeError(w, http.StatusMethodNotAllowed, "POST a session event")
		return
	}
	var req auth.Request
	if err := json.UnmarshalRead(http.MaxBytesReader(w, r.Body, maxRequestBytes), &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid session event")
		return
	}

	switch req.Event {
	case auth.EventConnect, auth.EventRevalidate:
		h.answer(w, req)
	case auth.EventEnd:
		slog.Info("authserver: session ended", "id", req.ID, "reason", req.Reason, "duration_s", req.Duration,
			"bytes_sent", req.Bytes.Sent, "bytes_received", req.Bytes.Received)
		w.WriteHeader(http.StatusNoContent)
	default:
		writeError(w, http.StatusBadRequest, "unknown event")
	}
}

// answer verifies the token in the request's query and writes its grant, or
// a refusal: 401 for a token that can't be accepted, 403 for a valid one that
// grants nothing usable.
func (h *Handler) answer(w http.ResponseWriter, req auth.Request) {
	g, err := h.grantFor(req)
	if err != nil {
		status := http.StatusUnauthorized
		if errors.Is(err, token.ErrForbidden) {
			status = http.StatusForbidden
		}
		// The query carries the credential; it is never logged.
		slog.Info("authserver: session refused", "id", req.ID, "event", req.Event,
			"transport", req.Transport, "remote", req.Remote, "path", req.Path, "reason", err)
		writeError(w, status, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.MarshalWrite(w, g); err != nil {
		slog.Warn("authserver: write grant", "id", req.ID, "error", err)
	}
}

func (h *Handler) grantFor(req auth.Request) (grant, error) {
	query, err := url.ParseQuery(req.Query)
	if err != nil {
		return grant{}, fmt.Errorf("%w: query: %v", token.ErrInvalid, err)
	}
	if !query.Has("jwt") {
		if len(h.Anonymous) == 0 {
			return grant{}, fmt.Errorf("%w: no credential: the client must connect with ?jwt=", token.ErrInvalid)
		}
		return grant{Publish: h.Anonymous, Subscribe: h.Anonymous}, nil
	}
	now := time.Now
	if h.now != nil {
		now = h.now
	}
	c, err := token.Verify(query.Get("jwt"), h.Keys, now())
	if err != nil {
		return grant{}, err
	}
	g := grant{
		Expires:    c.ExpiresAt.Add(token.Leeway).Unix(),
		Revalidate: int64(h.Revalidate / time.Second),
	}
	if c.Publish != "" {
		g.Publish = []string{c.Publish + "/**"}
	}
	if c.Subscribe != "" {
		g.Subscribe = []string{c.Subscribe + "/**"}
	}
	return g, nil
}

// ParsePatterns parses comma-separated subtree patterns, each "**" or a path
// followed by "/**", as the relay accepts them.
func ParsePatterns(raw string) ([]string, error) {
	var patterns []string
	for p := range strings.SplitSeq(raw, ",") {
		p = strings.TrimSpace(p)
		if !validPattern(p) {
			return nil, fmt.Errorf("pattern %q: want \"**\" or a path followed by \"/**\"", p)
		}
		patterns = append(patterns, p)
	}
	return patterns, nil
}

// validPattern reports whether s is a subtree pattern the relay accepts: "**",
// or a path with no empty, ".", ".." or "*" segments followed by "/**".
func validPattern(s string) bool {
	if s == "**" {
		return true
	}
	base, ok := strings.CutSuffix(s, "/**")
	if !ok || base == "" {
		return false
	}
	for seg := range strings.SplitSeq(base, "/") {
		if seg == "" || seg == "." || seg == ".." || strings.Contains(seg, "*") {
			return false
		}
	}
	return true
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.MarshalWrite(w, map[string]string{"error": msg}) // not actionable: the status is already sent
}
