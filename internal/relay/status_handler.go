package relay

import (
	"encoding/json"
	"net/http"
	"sort"
	"time"
)

// overlayPathStatus is the deploy-facing snapshot of one active broadcast path.
type overlayPathStatus struct {
	Path        string    `json:"path"`
	Active      bool      `json:"active"`
	Hops        int       `json:"hops"`
	RTTMs       int64     `json:"rtt_ms,omitempty"`
	BitrateBps  uint64    `json:"bitrate_bps"`
	Source      string    `json:"source,omitempty"`
	LastUpdated time.Time `json:"last_updated"`
}

// overlayStatus is a structured JSON snapshot for placement and route visibility.
type overlayStatus struct {
	Timestamp time.Time           `json:"timestamp"`
	Uptime    string              `json:"uptime"`
	Live      bool                `json:"live"`
	Ready     bool                `json:"ready"`
	Paths     []overlayPathStatus `json:"paths"`
}

// statusHandler manages health check state
type statusHandler struct {
	startTime time.Time
	server    *Server
}

// newStatusHandler creates a new health checker
func newStatusHandler() *statusHandler {
	return &statusHandler{
		startTime: time.Now(),
	}
}

// ServeHTTP implements http.Handler for health check endpoint
func (h *statusHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}

	_ = json.NewEncoder(w).Encode(map[string]any{
		"timestamp": time.Now(),
		"uptime":    time.Since(h.startTime).String(),
		"live":      true,
		"ready":     true,
	})
}

// ServeStatus exposes the current overlay snapshot to deploy tooling.
func (h *statusHandler) ServeStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}

	_ = json.NewEncoder(w).Encode(h.Overlay())
}

// Overlay builds a deploy-oriented snapshot of the active relay overlay.
func (h *statusHandler) Overlay() overlayStatus {
	resp := overlayStatus{
		Timestamp: time.Now(),
		Uptime:    time.Since(h.startTime).String(),
		Live:      true,
		Ready:     true,
	}
	if h.server == nil {
		return resp
	}
	paths := h.server.overlaySnapshot()
	resp.Paths = paths
	return resp
}

func (s *Server) overlaySnapshot() []overlayPathStatus {
	if s == nil {
		return nil
	}
	if s.pathStatus == nil {
		return nil
	}

	s.pathStatusMu.Lock()
	defer s.pathStatusMu.Unlock()

	items := make([]overlayPathStatus, 0, len(s.pathStatus))
	for _, status := range s.pathStatus {
		items = append(items, status)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Path < items[j].Path })
	return items
}
