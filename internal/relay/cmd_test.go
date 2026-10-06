package relay

import (
	"context"
	"flag"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSetupTLS tests TLS configuration setup (basic validation only)
func TestSetupTLSInvalidFiles(t *testing.T) {
	_, err := setupTLS("/nonexistent/cert.pem", "/nonexistent/key.pem")
	if err == nil {
		t.Error("Expected error for nonexistent certificate files, got nil")
	}
}

// TestSetupTLSEmptyPaths tests error handling for empty paths
func TestSetupTLSEmptyPaths(t *testing.T) {
	_, err := setupTLS("", "")
	if err == nil {
		t.Error("Expected error for empty certificate paths, got nil")
	}
}

// --- serveComponents tests ---

type mockServer struct {
	listenCalled   chan struct{}
	shutdownCalled chan struct{}
	listenErr      error
}

func newMockServer(listenErr error) *mockServer {
	return &mockServer{listenCalled: make(chan struct{}), shutdownCalled: make(chan struct{}), listenErr: listenErr}
}

func (m *mockServer) ListenAndServe() error {
	close(m.listenCalled)
	if m.listenErr != nil {
		return m.listenErr
	}
	// Block until Shutdown signals us to exit
	<-m.shutdownCalled
	return nil
}

func (m *mockServer) Shutdown(_ context.Context) error {
	// signal the listen goroutine to exit
	select {
	case <-m.shutdownCalled:
		// already closed
	default:
		close(m.shutdownCalled)
	}
	return nil
}

func TestServeComponents_ShutdownOnContextCancel(t *testing.T) {
	relayMock := newMockServer(nil)
	httpMock := newMockServer(nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Run serveComponents in background
	go func() { _ = serveComponents(ctx, relayMock, httpMock, 1*time.Second) }()

	// wait for both ListenAndServe to have been invoked
	<-relayMock.listenCalled
	<-httpMock.listenCalled

	// cancel context to trigger shutdown
	cancel()

	// verify Shutdown was called on both mocks
	select {
	case <-relayMock.shutdownCalled:
		// ok
	case <-time.After(500 * time.Millisecond):
		t.Fatal("relay shutdown was not called")
	}

	select {
	case <-httpMock.shutdownCalled:
		// ok
	case <-time.After(500 * time.Millisecond):
		t.Fatal("http shutdown was not called")
	}
}

func TestServeComponents_IgnoresImmediateListenError(t *testing.T) {
	// Relay.ListenAndServe returns an error immediately. serveComponents should
	// still wait for ctx cancellation and call Shutdown on the other server.
	relayMock := newMockServer(fmt.Errorf("listen failed"))
	httpMock := newMockServer(nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() { _ = serveComponents(ctx, relayMock, httpMock, 1*time.Second) }()

	// relayMock.listenCalled will be closed quickly even though it returned
	<-relayMock.listenCalled
	<-httpMock.listenCalled

	cancel()

	select {
	case <-httpMock.shutdownCalled:
		// ok
	case <-time.After(500 * time.Millisecond):
		t.Fatal("http shutdown was not called after context cancel")
	}
}

// panicServer simulates a server whose ListenAndServe panics.
// serveComponents should recover the panic and return an error.
type panicServer struct{}

func (p *panicServer) ListenAndServe() error          { panic("boom") }
func (p *panicServer) Shutdown(context.Context) error { return nil }

func TestServeComponents_ReturnsErrorOnPanic(t *testing.T) {
	relayPanic := &panicServer{}
	httpMock := newMockServer(nil)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- serveComponents(ctx, relayPanic, httpMock, 1*time.Second) }()

	select {
	case err := <-errCh:
		require.Error(t, err)
		assert.Contains(t, err.Error(), "panic")
	case <-time.After(500 * time.Millisecond):
		t.Fatal("serveComponents did not return after panic")
	}
}

func TestRun_InvalidGroupCacheSize(t *testing.T) {
	t.Setenv("RELAY_ADDR", "localhost:4433")
	t.Setenv("GROUP_CACHE_SIZE", "not-a-number")

	err := Run(nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "GROUP_CACHE_SIZE")
}

// A relay still pointed at an auth server refuses to start, rather than
// starting with auth off.
func TestRun_RefusesQUMOAuthURL(t *testing.T) {
	t.Setenv("QUMO_AUTH_URL", "http://127.0.0.1:4440/")

	err := Run(nil)

	assert.ErrorContains(t, err, "QUMO_AUTH_URL is no longer supported")
}

// A relay told to dial peers without a CA and a peer identity stops at
// startup rather than dialing with the wrong certificate.
func TestRun_RefusesPeersWithoutPeerTrust(t *testing.T) {
	t.Setenv("PEERS", "hub:4433")
	t.Setenv("CA_FILE", "")
	t.Setenv("PEER_CERT_FILE", "")
	t.Setenv("PEER_KEY_FILE", "")

	err := Run(nil)

	assert.ErrorContains(t, err, "PEERS needs CA_FILE, PEER_CERT_FILE and PEER_KEY_FILE")
}

// TestParseRelayArgs covers the --role flag: it is the only execution-mode
// flag (secrets/deployment config stay env), and it is flag-only — there is no
// ROLE env fallback to misconfigure against.
func TestParseRelayArgs(t *testing.T) {
	cases := map[string]struct {
		args     []string
		wantRole string
		wantErr  string // non-empty → expect an error containing this substring
		wantHelp bool
	}{
		"no flags → flat":       {args: nil, wantRole: ""},
		"--role hub":            {args: []string{"--role", "hub"}, wantRole: "hub"},
		"--role=edge":           {args: []string{"--role=edge"}, wantRole: "edge"},
		"positional rejected":   {args: []string{"hub"}, wantErr: "unexpected argument"},
		"unknown flag rejected": {args: []string{"--bogus"}, wantErr: "not defined"},
		"-h renders help":       {args: []string{"-h"}, wantHelp: true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			flags, err := parseRelayArgs(tc.args)
			if tc.wantHelp {
				require.ErrorIs(t, err, flag.ErrHelp)
				return
			}
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantRole, flags.Role)
		})
	}
}
