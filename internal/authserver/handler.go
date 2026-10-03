package authserver

import (
	"encoding/json/v2"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"time"
)

// maxRequestBytes bounds a relay's session-event body.
const maxRequestBytes = 64 << 10

// Session events a relay sends (moq-auth's Request.event).
const (
	eventConnect    = "connect"
	eventRevalidate = "revalidate"
	eventEnd        = "end"
)

// request is the subset of a relay's session event the auth server reads.
// Other members (node, local, server_name, bytes, ...) are accepted and
// ignored for now.
type request struct {
	ID        string `json:"id"`
	Event     string `json:"event"`
	Transport string `json:"transport"`
	Remote    string `json:"remote"`
	Path      string `json:"path"`
	Query     string `json:"query"`
	Reason    string `json:"reason"`
}

// Handler answers a relay's session events: connect and revalidate get a
// grant or a refusal, end is acknowledged.
type Handler struct {
	// Keys are the trusted signing keys, by kid.
	Keys map[string]Key
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
	var req request
	if err := json.UnmarshalRead(http.MaxBytesReader(w, r.Body, maxRequestBytes), &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid session event")
		return
	}

	switch req.Event {
	case eventConnect, eventRevalidate:
		h.answer(w, req)
	case eventEnd:
		slog.Info("authserver: session ended", "id", req.ID, "reason", req.Reason)
		w.WriteHeader(http.StatusNoContent)
	default:
		writeError(w, http.StatusBadRequest, "unknown event")
	}
}

// answer verifies the token in the request's query and writes its grant.
func (h *Handler) answer(w http.ResponseWriter, req request) {
	g, err := h.grantForRequest(req)
	var refused refusal
	if errors.As(err, &refused) {
		// The query carries the credential; it is never logged.
		slog.Info("authserver: session refused", "id", req.ID, "event", req.Event,
			"transport", req.Transport, "remote", req.Remote, "path", req.Path, "reason", refused.reason)
		writeError(w, refused.status, refused.reason)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.MarshalWrite(w, g); err != nil {
		slog.Warn("authserver: write grant", "id", req.ID, "error", err)
	}
}

func (h *Handler) grantForRequest(req request) (grant, error) {
	query, err := url.ParseQuery(req.Query)
	if err != nil {
		return grant{}, refuse("query: %v", err)
	}
	if !query.Has("jwt") {
		if len(h.Anonymous) == 0 {
			return grant{}, refuse("no credential: the client must connect with ?jwt=")
		}
		return grant{Publish: h.Anonymous, Subscribe: h.Anonymous}, nil
	}
	now := time.Now
	if h.now != nil {
		now = h.now
	}
	c, key, err := verify(query.Get("jwt"), h.Keys, now())
	if err != nil {
		return grant{}, err
	}
	return grantFor(c, key, h.Revalidate)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.MarshalWrite(w, map[string]string{"error": msg})
}
