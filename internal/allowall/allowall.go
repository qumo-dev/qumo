// Package allowall serves an auth server that admits every session to
// everything. Local tools that start their own relay (the playground, the
// benchmark harnesses) point the relay's QUMO_AUTH_URL at it. It verifies
// nothing and holds no policy: a deployment runs a real auth server.
package allowall

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// grant opens every path to publish and subscribe.
const grant = `{"publish":["**"],"subscribe":["**"]}`

const (
	headerTimeout   = 5 * time.Second
	shutdownTimeout = 5 * time.Second
)

// Serve serves on a new loopback port until ctx ends, and returns the URL a
// relay's QUMO_AUTH_URL takes.
func Serve(ctx context.Context) (string, error) {
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("allowall: listen: %w", err)
	}
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, grant) // not actionable: the relay sees a failed request and refuses
		}),
		ReadHeaderTimeout: headerTimeout,
	}
	go func() {
		_ = srv.Serve(ln) // not actionable: returns ErrServerClosed after Shutdown below
	}()
	context.AfterFunc(ctx, func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx) // not actionable: the program is ending
	})
	return "http://" + ln.Addr().String() + "/", nil
}
