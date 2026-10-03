package controller

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ProcessState holds the running state of a relay subprocess.
type ProcessState struct {
	Node    *RelayNode
	Cmd     *exec.Cmd
	Started time.Time
	Done    chan struct{} // closed when process exits
}

// httpClient lazily returns an HTTP client with a 5-second timeout.
// Wrapped in a function to avoid mutable package-level state (Go skill rule 1).
func httpClient() *http.Client {
	return &http.Client{Timeout: 5 * time.Second}
}

// startRelay launches one relay process with the given configuration.
func startRelay(ctx context.Context, bin string, node *RelayNode, certDir string, top *Topology) (*ProcessState, error) {
	args := []string{"relay"}
	env := os.Environ()
	env = append(env,
		fmt.Sprintf("RELAY_ADDR=127.0.0.1:%d", node.Port),
		fmt.Sprintf("CERT_FILE=%s", filepath.Join(certDir, "cert.pem")),
		fmt.Sprintf("KEY_FILE=%s", filepath.Join(certDir, "key.pem")),
		fmt.Sprintf("CA_FILE=%s", filepath.Join(certDir, "cert.pem")),
		fmt.Sprintf("RELAY_NAME=%s", node.Name),
		"RELAY_GOGC=800",
		"GROUP_CACHE_SIZE=8",
		"LOCAL_RESOLVER_INTERVAL=0s",
		"QUMO_AUTH_PUBLIC=**", // benchmarks run open
	)
	if node.PeerAddr != "" {
		env = append(env, fmt.Sprintf("PEERS=%s", node.PeerAddr))
	}

	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = env
	cmd.Dir = certDir // relay reads cert.pem/key.pem relative to CWD
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	// Core pinning (taskset) on Linux.
	if top.Cfg.PinRelays {
		mask := top.CoreRange(node)
		if mask != "" {
			if _, err := exec.LookPath("taskset"); err == nil {
				// Wrap with taskset.
				cmd = exec.CommandContext(ctx, "taskset", "-c", mask, bin)
				cmd.Args = append(cmd.Args, args...)
				cmd.Env = env
				cmd.Dir = certDir
				cmd.Stdout = os.Stdout
				cmd.Stderr = os.Stderr
				slog.Debug("pinning relay to cores", "node", node.Name, "mask", mask)
			}
		}
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", node.Name, err)
	}

	ps := &ProcessState{
		Node:    node,
		Cmd:     cmd,
		Started: time.Now(),
		Done:    make(chan struct{}),
	}
	go func() {
		_ = cmd.Wait()
		close(ps.Done)
	}()

	slog.Info("relay started", "node", node.Name, "port", node.Port, "pid", cmd.Process.Pid)
	return ps, nil
}

// stopRelay sends a graceful SIGTERM to a relay process and waits for it to
// exit within the timeout, then SIGKILLs survivors.
func stopRelay(ps *ProcessState, timeout time.Duration) {
	if ps == nil || ps.Cmd == nil || ps.Cmd.Process == nil {
		return
	}
	pid := ps.Cmd.Process.Pid
	slog.Info("stopping relay", "node", ps.Node.Name, "pid", pid)

	proc, err := os.FindProcess(pid)
	if err != nil {
		return
	}
	_ = proc.Signal(syscall.SIGTERM)

	graceTimer := time.NewTimer(timeout)
	defer graceTimer.Stop()
	select {
	case <-ps.Done:
		slog.Info("relay stopped gracefully", "node", ps.Node.Name)
		return
	case <-graceTimer.C:
	}

	_ = proc.Signal(syscall.SIGKILL)
	killTimer := time.NewTimer(2 * time.Second)
	defer killTimer.Stop()
	select {
	case <-ps.Done:
		slog.Info("relay killed", "node", ps.Node.Name)
	case <-killTimer.C:
		slog.Warn("relay did not respond to SIGKILL", "node", ps.Node.Name)
	}
}

// waitReady polls the relay's /health endpoint until it responds 200 OK.
func waitReady(ctx context.Context, ps *ProcessState, timeout time.Duration) error {
	url := fmt.Sprintf("http://127.0.0.1:%d/health", ps.Node.Port)
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()

	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()

	for {
		if err := getOK(ctx, url); err == nil {
			slog.Debug("relay ready", "node", ps.Node.Name, "port", ps.Node.Port)
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("timeout waiting for %s (port %d) to become ready", ps.Node.Name, ps.Node.Port)
		case <-tick.C:
		}
	}
}

// getOK performs a GET and returns nil if the status code is 200.
func getOK(ctx context.Context, url string) error {
	childCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(childCtx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}

// fetchMetricsBody fetches the /metrics endpoint and returns the body bytes.
func fetchMetricsBody(ctx context.Context, port int) ([]byte, error) {
	url := fmt.Sprintf("http://127.0.0.1:%d/metrics", port)
	childCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(childCtx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	return body, nil
}

// waitPeerConnected polls /metrics until qumo_relay_peers_connected > 0.
func waitPeerConnected(ctx context.Context, ps *ProcessState, timeout time.Duration) error {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()

	for {
		body, err := fetchMetricsBody(ctx, ps.Node.Port)
		if err == nil {
			for line := range strings.SplitSeq(string(body), "\n") {
				if strings.HasPrefix(line, "qumo_relay_peers_connected") {
					parts := strings.Fields(line)
					if len(parts) == 2 {
						if n, err := strconv.Atoi(parts[1]); err == nil && n > 0 {
							slog.Debug("peer connected", "node", ps.Node.Name, "count", n)
							return nil
						}
					}
				}
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("timeout waiting for peer connection on %s", ps.Node.Name)
		case <-tick.C:
		}
	}
}

// waitBroadcastActive polls /metrics until qumo_relay_broadcasts_active > 0.
func waitBroadcastActive(ctx context.Context, ps *ProcessState, timeout time.Duration) error {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()

	for {
		body, err := fetchMetricsBody(ctx, ps.Node.Port)
		if err == nil {
			for line := range strings.SplitSeq(string(body), "\n") {
				if strings.HasPrefix(line, "qumo_relay_broadcasts_active") {
					parts := strings.Fields(line)
					if len(parts) == 2 {
						if n, err := strconv.Atoi(parts[1]); err == nil && n > 0 {
							slog.Debug("broadcast active", "node", ps.Node.Name, "count", n)
							return nil
						}
					}
				}
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("timeout waiting for broadcast on %s", ps.Node.Name)
		case <-tick.C:
		}
	}
}

// killPortProcesses kills any process listening on the given TCP port. It tries
// multiple strategies in order of reliability and logs each attempt. After
// killing, it waits for the port to become free with a short retry loop.
func killPortProcesses(port int) {
	portStr := fmt.Sprintf("%d", port)

	// Strategy 1: fuser -k (fast, targets port directly).
	if _, err := exec.LookPath("fuser"); err == nil {
		cmd := exec.Command("fuser", "-k", portStr+"/tcp")
		if out, err := cmd.CombinedOutput(); err != nil {
			slog.Debug("killPortProcesses: fuser -k failed", "port", port, "err", err, "output", strings.TrimSpace(string(out)))
		} else {
			slog.Debug("killPortProcesses: fuser -k succeeded", "port", port, "output", strings.TrimSpace(string(out)))
		}
	}

	// Strategy 2: lsof -ti :port | xargs kill -9.
	if _, err := exec.LookPath("lsof"); err == nil {
		// List PIDs with -F p for parser-friendly output, pipe to kill.
		cmd := exec.Command("sh", "-c", fmt.Sprintf("lsof -ti :%s 2>/dev/null | xargs -r kill -9 2>/dev/null", portStr))
		if out, err := cmd.CombinedOutput(); err != nil {
			slog.Debug("killPortProcesses: lsof fallback failed", "port", port, "err", err, "output", strings.TrimSpace(string(out)))
		} else {
			slog.Debug("killPortProcesses: lsof fallback succeeded", "port", port, "output", strings.TrimSpace(string(out)))
		}
	}

	// Wait for the port to become free (up to ~3 seconds).
	waitPortClosed(port, 3*time.Second)
}

// waitPortClosed polls until no process is listening on the given TCP port or
// the timeout expires. It tries a TCP dial first, then falls back to ss/lsof.
func waitPortClosed(port int, timeout time.Duration) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()

	poll := time.NewTicker(200 * time.Millisecond)
	defer poll.Stop()

	for {
		// Quick check: can we connect?
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 500*time.Millisecond)
		if err != nil {
			// Connection refused → port is free.
			return
		}
		_ = conn.Close()

		select {
		case <-deadline.C:
			slog.Warn("killPortProcesses: port still in use after timeout", "port", port)
			return
		case <-poll.C:
			// Retry.
		}
	}
}
