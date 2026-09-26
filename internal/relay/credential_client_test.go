package relay

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestCredentialClient wires a CredentialClient to srv with an auth token pre-set.
func newTestCredentialClient(srv *httptest.Server) *CredentialClient {
	return &CredentialClient{
		baseURL:    srv.URL,
		authToken:  "test-token",
		httpClient: srv.Client(),
		cache:      make(map[string]cachedCredential),
	}
}

// writeValidIntrospect writes a valid credential introspection response.
func writeValidIntrospect(w http.ResponseWriter, tokenID string, revalidateAfter time.Time) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(struct {
		Valid           bool   `json:"valid"`
		TokenID         string `json:"token_id"`
		ProjectID       string `json:"project_id"`
		TenantID        string `json:"tenant_id"`
		APIKeyID        string `json:"api_key_id"`
		Environment     string `json:"environment"`
		RevalidateAfter string `json:"revalidate_after"`
	}{
		Valid:           true,
		TokenID:         tokenID,
		ProjectID:       "proj-1",
		TenantID:        "tenant-1",
		APIKeyID:        "key-1",
		Environment:     "production",
		RevalidateAfter: revalidateAfter.UTC().Format(time.RFC3339),
	})
}

// ── NewCredentialClient ──────────────────────────────────────────────────────────

func TestNewCredentialClient(t *testing.T) {
	t.Run("nil when QUMO_CREDENTIAL_URL unset", func(t *testing.T) {
		t.Setenv("QUMO_CREDENTIAL_URL", "")
		assert.Nil(t, NewCredentialClient())
	})

	t.Run("returns client with correct fields", func(t *testing.T) {
		t.Setenv("QUMO_CREDENTIAL_URL", "https://credential.example.com")
		t.Setenv("QUMO_RELAY_TOKEN", "my-secret")
		c := NewCredentialClient()
		require.NotNil(t, c)
		assert.Equal(t, "https://credential.example.com", c.baseURL)
		assert.Equal(t, "my-secret", c.authToken)
	})

	t.Run("adds https scheme when missing", func(t *testing.T) {
		t.Setenv("QUMO_CREDENTIAL_URL", "credential.example.com")
		t.Setenv("QUMO_RELAY_TOKEN", "tok")
		c := NewCredentialClient()
		require.NotNil(t, c)
		assert.Equal(t, "https://credential.example.com", c.baseURL)
	})

	t.Run("preserves http scheme", func(t *testing.T) {
		t.Setenv("QUMO_CREDENTIAL_URL", "http://credential.example.com")
		t.Setenv("QUMO_RELAY_TOKEN", "tok")
		c := NewCredentialClient()
		require.NotNil(t, c)
		assert.Equal(t, "http://credential.example.com", c.baseURL)
	})

	t.Run("strips trailing slash", func(t *testing.T) {
		t.Setenv("QUMO_CREDENTIAL_URL", "https://credential.example.com/")
		t.Setenv("QUMO_RELAY_TOKEN", "tok")
		c := NewCredentialClient()
		require.NotNil(t, c)
		assert.Equal(t, "https://credential.example.com", c.baseURL)
	})
}

// ── Introspect – request shape ────────────────────────────────────────────────

func TestCredentialClient_Introspect_RequestShape(t *testing.T) {
	revalidate := time.Now().Add(10 * time.Minute)
	var gotMethod, gotPath, gotAuth, gotCT string
	var gotBodyBytes []byte

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotCT = r.Header.Get("Content-Type")
		gotBodyBytes, _ = io.ReadAll(r.Body)
		writeValidIntrospect(w, "tok-1", revalidate)
	}))
	defer srv.Close()

	_, err := newTestCredentialClient(srv).Introspect(context.Background(), "the-jwt", "/tenant/project/live")
	require.NoError(t, err)

	assert.Equal(t, http.MethodPost, gotMethod)
	assert.Equal(t, "/v1/credentials/introspect", gotPath)
	assert.Equal(t, "Bearer test-token", gotAuth)
	assert.Equal(t, "application/json", gotCT)
	var body map[string]string
	require.NoError(t, json.Unmarshal(gotBodyBytes, &body))
	assert.Equal(t, map[string]string{"token": "the-jwt", "broadcast_path": "/tenant/project/live"}, body,
		"the relay forwards the announced path; the control plane decides")
}

func TestCredentialClient_Introspect_NoAuthHeaderWhenTokenEmpty(t *testing.T) {
	revalidate := time.Now().Add(10 * time.Minute)
	var gotAuth string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		writeValidIntrospect(w, "tok", revalidate)
	}))
	defer srv.Close()

	c := newTestCredentialClient(srv)
	c.authToken = ""
	_, err := c.Introspect(context.Background(), "jwt", "/tenant/project/live")
	require.NoError(t, err)
	assert.Empty(t, gotAuth)
}

// ── Introspect – result parsing ───────────────────────────────────────────────

func TestCredentialClient_Introspect_ValidCredential(t *testing.T) {
	revalidate := time.Now().Add(5 * time.Minute)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeValidIntrospect(w, "tok-abc", revalidate)
	}))
	defer srv.Close()

	result, err := newTestCredentialClient(srv).Introspect(context.Background(), "jwt", "/tenant/project/live")
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, "tok-abc", result.TokenID)
	assert.Equal(t, "proj-1", result.ProjectID)
	assert.Equal(t, "tenant-1", result.TenantID)
	assert.Equal(t, "key-1", result.APIKeyID)
	assert.Equal(t, "production", result.Environment)
}

func TestCredentialClient_Introspect_InvalidCredential(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"valid":false}`))
	}))
	defer srv.Close()

	result, err := newTestCredentialClient(srv).Introspect(context.Background(), "bad-jwt", "/tenant/project/live")
	require.NoError(t, err)
	assert.Nil(t, result, "valid:false must return nil result without an error")
}

// ── Introspect – caching ──────────────────────────────────────────────────────

func TestCredentialClient_Introspect_CacheHit(t *testing.T) {
	var calls atomic.Int32
	revalidate := time.Now().Add(10 * time.Minute)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		writeValidIntrospect(w, "tok-cache", revalidate)
	}))
	defer srv.Close()

	c := newTestCredentialClient(srv)
	ctx := context.Background()

	r1, err := c.Introspect(ctx, "jwt-same", "/tenant/project/live")
	require.NoError(t, err)
	require.NotNil(t, r1)

	r2, err := c.Introspect(ctx, "jwt-same", "/tenant/project/live")
	require.NoError(t, err)
	require.NotNil(t, r2)

	assert.Equal(t, int32(1), calls.Load(), "second call must be served from cache without an HTTP round-trip")
	assert.Equal(t, r1.TokenID, r2.TokenID)
}

func TestCredentialClient_Introspect_DifferentJWTsAreCachedIndependently(t *testing.T) {
	var calls atomic.Int32
	revalidate := time.Now().Add(10 * time.Minute)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		writeValidIntrospect(w, "tok-"+string(rune('A'+n-1)), revalidate)
	}))
	defer srv.Close()

	c := newTestCredentialClient(srv)
	ctx := context.Background()

	r1, err := c.Introspect(ctx, "jwt-alpha", "/tenant/project/live")
	require.NoError(t, err)

	r2, err := c.Introspect(ctx, "jwt-beta", "/tenant/project/live")
	require.NoError(t, err)

	assert.Equal(t, int32(2), calls.Load(), "distinct JWTs must each make their own HTTP request")
	assert.NotEqual(t, r1.TokenID, r2.TokenID)
}

// TestCredentialClient_Introspect_SameJWTDifferentPathsAreNotShared guards the
// cache key: a result for one broadcast path must never answer for another,
// or a credential valid for its own path would be accepted on any path once
// cached.
func TestCredentialClient_Introspect_SameJWTDifferentPathsAreNotShared(t *testing.T) {
	var calls atomic.Int32
	var paths []string
	var mu sync.Mutex
	revalidate := time.Now().Add(10 * time.Minute)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body) // not actionable: asserted via paths
		mu.Lock()
		paths = append(paths, body["broadcast_path"])
		mu.Unlock()
		writeValidIntrospect(w, "tok-1", revalidate)
	}))
	defer srv.Close()

	c := newTestCredentialClient(srv)
	ctx := context.Background()

	_, err := c.Introspect(ctx, "jwt-1", "/tenant-a/project/live")
	require.NoError(t, err)
	_, err = c.Introspect(ctx, "jwt-1", "/tenant-b/project/live")
	require.NoError(t, err)
	_, err = c.Introspect(ctx, "jwt-1", "/tenant-a/project/live")
	require.NoError(t, err)

	assert.Equal(t, int32(2), calls.Load(), "one request per (JWT, path); the repeat is a cache hit")
	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []string{"/tenant-a/project/live", "/tenant-b/project/live"}, paths)
}

func TestCredentialClient_Introspect_CacheExpiry(t *testing.T) {
	var calls atomic.Int32
	// Returning revalidate_after in the past causes the fallback TTL; but even
	// with the 5-min fallback the entry won't be immediately re-fetched. Instead
	// we manually insert a pre-expired entry to test the expiry path.
	revalidate := time.Now().Add(10 * time.Minute)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		writeValidIntrospect(w, "tok-exp", revalidate)
	}))
	defer srv.Close()

	c := newTestCredentialClient(srv)

	// Seed the cache with an already-expired entry.
	c.cacheMu.Lock()
	c.cache["jwt-exp"] = cachedCredential{
		result:  IntrospectResult{TokenID: "stale"},
		expires: time.Now().Add(-1 * time.Minute),
	}
	c.cacheMu.Unlock()

	result, err := c.Introspect(context.Background(), "jwt-exp", "/tenant/project/live")
	require.NoError(t, err)
	require.NotNil(t, result)

	assert.Equal(t, int32(1), calls.Load(), "expired entry must trigger a fresh HTTP request")
	assert.Equal(t, "tok-exp", result.TokenID, "result must come from the fresh response, not the stale cache")
}

func TestCredentialClient_Introspect_CacheEviction(t *testing.T) {
	revalidate := time.Now().Add(10 * time.Minute)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeValidIntrospect(w, "tok-new", revalidate)
	}))
	defer srv.Close()

	c := newTestCredentialClient(srv)

	// Manually insert two stale entries.
	c.cacheMu.Lock()
	c.cache["stale-1"] = cachedCredential{expires: time.Now().Add(-2 * time.Minute)}
	c.cache["stale-2"] = cachedCredential{expires: time.Now().Add(-1 * time.Minute)}
	c.cacheMu.Unlock()

	// A successful introspection triggers the sweep.
	_, err := c.Introspect(context.Background(), "fresh-jwt", "/tenant/project/live")
	require.NoError(t, err)

	c.cacheMu.Lock()
	_, stale1 := c.cache["stale-1"]
	_, stale2 := c.cache["stale-2"]
	_, fresh := c.cache[introspectCacheKey("fresh-jwt", "/tenant/project/live")]
	c.cacheMu.Unlock()

	assert.False(t, stale1, "expired entry stale-1 must be swept")
	assert.False(t, stale2, "expired entry stale-2 must be swept")
	assert.True(t, fresh, "fresh entry must remain in cache")
}

// ── Introspect – singleflight ─────────────────────────────────────────────────

func TestCredentialClient_Introspect_SingleflightCoalescesConcurrentRequests(t *testing.T) {
	const n = 10
	var calls atomic.Int32
	revalidate := time.Now().Add(10 * time.Minute)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		// Hold the response long enough for all goroutines to pile up in singleflight.
		time.Sleep(50 * time.Millisecond)
		writeValidIntrospect(w, "tok-sf", revalidate)
	}))
	defer srv.Close()

	c := newTestCredentialClient(srv)
	ctx := context.Background()

	var wg sync.WaitGroup
	wg.Add(n)
	results := make([]*IntrospectResult, n)
	for i := range n {
		go func(i int) {
			defer wg.Done()
			results[i], _ = c.Introspect(ctx, "shared-jwt", "/tenant/project/live")
		}(i)
	}
	wg.Wait()

	assert.Equal(t, int32(1), calls.Load(), "singleflight must coalesce all concurrent requests into one HTTP call")
	for i, r := range results {
		require.NotNil(t, r, "goroutine %d must receive a non-nil result", i)
		assert.Equal(t, "tok-sf", r.TokenID)
	}
}

// ── Introspect – error handling ───────────────────────────────────────────────

func TestCredentialClient_Introspect_NonOKStatus(t *testing.T) {
	for _, status := range []int{
		http.StatusUnauthorized,
		http.StatusForbidden,
		http.StatusInternalServerError,
		http.StatusBadGateway,
	} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
			}))
			defer srv.Close()

			_, err := newTestCredentialClient(srv).Introspect(context.Background(), "jwt", "/tenant/project/live")
			require.Error(t, err)
			assert.Contains(t, err.Error(), "credential:")
		})
	}
}

func TestCredentialClient_Introspect_MalformedJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{not valid json"))
	}))
	defer srv.Close()

	_, err := newTestCredentialClient(srv).Introspect(context.Background(), "jwt", "/tenant/project/live")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "credential:")
}

func TestCredentialClient_Introspect_ContextCancelled(t *testing.T) {
	// Pre-cancel the context so the HTTP client rejects the request immediately,
	// avoiding any reliance on the server-side request context being cancelled
	// when the client drops the connection (behaviour differs across platforms).
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK) // should never be reached
	}))
	defer srv.Close()

	_, err := newTestCredentialClient(srv).Introspect(ctx, "jwt", "/tenant/project/live")
	require.Error(t, err)
}

// ── ReportUsage ───────────────────────────────────────────────────────────────

func TestCredentialClient_ReportUsage_SendsCorrectPayload(t *testing.T) {
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
			BroadcastSessionID: "sess-abc",
			OwnerTokenID:       "tok-1",
			Metrics: map[string]int64{
				"gateway.ingress_bytes": 1024,
				"gateway.egress_bytes":  4096,
			},
			Ts: time.Now().UTC().Format(time.RFC3339),
		},
	}

	err := newTestCredentialClient(srv).ReportUsage(context.Background(), events)
	require.NoError(t, err)

	assert.Equal(t, http.MethodPost, gotMethod)
	assert.Equal(t, "/v1/usage/events", gotPath)
	assert.Equal(t, "Bearer test-token", gotAuth)
	assert.Equal(t, "application/json", gotCT)
	require.Len(t, gotEvents, 1)
	assert.Equal(t, "sess-abc", gotEvents[0].BroadcastSessionID)
	assert.Equal(t, int64(1024), gotEvents[0].Metrics["gateway.ingress_bytes"])
	assert.Equal(t, int64(4096), gotEvents[0].Metrics["gateway.egress_bytes"])
}

func TestCredentialClient_ReportUsage_NoOpOnEmptySlice(t *testing.T) {
	var called bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	defer srv.Close()

	err := newTestCredentialClient(srv).ReportUsage(context.Background(), nil)
	require.NoError(t, err)
	assert.False(t, called, "must not make an HTTP request for an empty event list")

	err = newTestCredentialClient(srv).ReportUsage(context.Background(), []UsageEvent{})
	require.NoError(t, err)
	assert.False(t, called, "must not make an HTTP request for an empty event list")
}

func TestCredentialClient_ReportUsage_NonOKStatus(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
			}))
			defer srv.Close()

			err := newTestCredentialClient(srv).ReportUsage(context.Background(), []UsageEvent{{BroadcastSessionID: "x"}})
			require.Error(t, err)
			assert.Contains(t, err.Error(), "credential:")
		})
	}
}

func TestCredentialClient_ReportUsage_ContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel so the HTTP dial is rejected immediately

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK) // should never be reached
	}))
	defer srv.Close()

	err := newTestCredentialClient(srv).ReportUsage(ctx, []UsageEvent{{BroadcastSessionID: "x"}})
	require.Error(t, err)
}
