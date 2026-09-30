package relay

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestUsageClient wires a usageClient to srv with an auth token pre-set.
func newTestUsageClient(srv *httptest.Server) *usageClient {
	return &usageClient{
		baseURL:    srv.URL,
		authToken:  "test-token",
		httpClient: srv.Client(),
	}
}

// ── newUsageClient ────────────────────────────────────────────────────────────

func TestNewUsageClient(t *testing.T) {
	t.Run("nil when QUMO_CREDENTIAL_URL unset", func(t *testing.T) {
		t.Setenv("QUMO_CREDENTIAL_URL", "")
		assert.Nil(t, newUsageClient())
	})

	t.Run("returns client with correct fields", func(t *testing.T) {
		t.Setenv("QUMO_CREDENTIAL_URL", "https://credential.example.com")
		t.Setenv("QUMO_RELAY_TOKEN", "my-secret")
		c := newUsageClient()
		require.NotNil(t, c)
		assert.Equal(t, "https://credential.example.com", c.baseURL)
		assert.Equal(t, "my-secret", c.authToken)
	})

	t.Run("adds https scheme when missing", func(t *testing.T) {
		t.Setenv("QUMO_CREDENTIAL_URL", "credential.example.com")
		t.Setenv("QUMO_RELAY_TOKEN", "tok")
		c := newUsageClient()
		require.NotNil(t, c)
		assert.Equal(t, "https://credential.example.com", c.baseURL)
	})

	t.Run("preserves http scheme", func(t *testing.T) {
		t.Setenv("QUMO_CREDENTIAL_URL", "http://credential.example.com")
		t.Setenv("QUMO_RELAY_TOKEN", "tok")
		c := newUsageClient()
		require.NotNil(t, c)
		assert.Equal(t, "http://credential.example.com", c.baseURL)
	})

	t.Run("strips trailing slash", func(t *testing.T) {
		t.Setenv("QUMO_CREDENTIAL_URL", "https://credential.example.com/")
		t.Setenv("QUMO_RELAY_TOKEN", "tok")
		c := newUsageClient()
		require.NotNil(t, c)
		assert.Equal(t, "https://credential.example.com", c.baseURL)
	})
}

// ── ReportUsage ───────────────────────────────────────────────────────────────

func TestUsageClient_ReportUsage_SendsCorrectPayload(t *testing.T) {
	var gotMethod, gotPath, gotAuth, gotCT string
	var gotEvents []UsageEvent

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotCT = r.Header.Get("Content-Type")
		_ = json.NewDecoder(r.Body).Decode(&gotEvents)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	events := []UsageEvent{
		{
			SessionID: "sess-abc",
			KeyID:     "kid-1",
			Metrics: map[string]int64{
				"gateway.ingress_bytes": 1024,
				"gateway.egress_bytes":  4096,
			},
			Ts: time.Now().UTC().Format(time.RFC3339),
		},
	}

	err := newTestUsageClient(srv).ReportUsage(context.Background(), events)
	require.NoError(t, err)

	assert.Equal(t, http.MethodPost, gotMethod)
	assert.Equal(t, "/v1/usage/events", gotPath)
	assert.Equal(t, "Bearer test-token", gotAuth)
	assert.Equal(t, "application/json", gotCT)
	require.Len(t, gotEvents, 1)
	assert.Equal(t, "sess-abc", gotEvents[0].SessionID)
	assert.Equal(t, "kid-1", gotEvents[0].KeyID)
	assert.Equal(t, int64(1024), gotEvents[0].Metrics["gateway.ingress_bytes"])
	assert.Equal(t, int64(4096), gotEvents[0].Metrics["gateway.egress_bytes"])
}

func TestUsageClient_ReportUsage_NoOpOnEmptySlice(t *testing.T) {
	var called bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	defer srv.Close()

	err := newTestUsageClient(srv).ReportUsage(context.Background(), nil)
	require.NoError(t, err)
	assert.False(t, called, "must not make an HTTP request for an empty event list")

	err = newTestUsageClient(srv).ReportUsage(context.Background(), []UsageEvent{})
	require.NoError(t, err)
	assert.False(t, called, "must not make an HTTP request for an empty event list")
}

func TestUsageClient_ReportUsage_NonOKStatus(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
			}))
			defer srv.Close()

			err := newTestUsageClient(srv).ReportUsage(context.Background(), []UsageEvent{{SessionID: "x"}})
			require.Error(t, err)
			assert.Contains(t, err.Error(), "usage:")
		})
	}
}

func TestUsageClient_ReportUsage_ContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel so the HTTP dial is rejected immediately

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK) // should never be reached
	}))
	defer srv.Close()

	err := newTestUsageClient(srv).ReportUsage(ctx, []UsageEvent{{SessionID: "x"}})
	require.Error(t, err)
}
