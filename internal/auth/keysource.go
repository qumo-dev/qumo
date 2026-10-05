package auth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"
)

const (
	// keysRefresh is how often the key set is refreshed: a new key works,
	// and a withdrawn one stops, within one refresh.
	keysRefresh = 30 * time.Second
	// keysMaxStale is how long the relay admits new sessions on its last key
	// set while it can't refresh it (fail-static). Live sessions aren't
	// affected: they run to their expiry either way.
	keysMaxStale = 6 * time.Hour
	// keysFetchTimeout bounds one download.
	keysFetchTimeout = 10 * time.Second
	// maxKeySetBytes bounds a downloaded key set.
	maxKeySetBytes = 32 << 20
)

// keyStore holds the current key set and when it was last refreshed. A key
// source writes it; the verifier reads it.
type keyStore struct {
	mu          sync.RWMutex
	set         *keySet
	lastSuccess time.Time
}

// current returns the key set, nil before the first successful load, and
// whether it is fresh enough to admit new sessions at now.
func (s *keyStore) current(now time.Time) (*keySet, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.set, s.set != nil && now.Sub(s.lastSuccess) <= keysMaxStale
}

func (s *keyStore) replace(set *keySet, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.set, s.lastSuccess = set, now
}

func (s *keyStore) confirm(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastSuccess = now
}

// keySource refreshes a keyStore once.
type keySource interface {
	refresh(ctx context.Context, store *keyStore, now time.Time) error
	// String names the source in logs.
	String() string
}

// runKeySource refreshes store from src now and then every keysRefresh until
// ctx ends. The first failure after a success is logged as an error, repeats
// quietly, and the recovery as info: the last set stays in use throughout.
func runKeySource(ctx context.Context, src keySource, store *keyStore, now func() time.Time) {
	ticker := time.NewTicker(keysRefresh)
	defer ticker.Stop()
	failing := false
	for {
		err := src.refresh(ctx, store, now())
		switch {
		case err != nil && ctx.Err() != nil:
			return
		case err != nil && !failing:
			failing = true
			slog.Error("relay: key set refresh failed; keeping the last key set", "source", src.String(), "error", err)
		case err != nil:
			slog.Debug("relay: key set refresh still failing", "source", src.String(), "error", err)
		case failing:
			failing = false
			slog.Info("relay: key set refresh recovered", "source", src.String())
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// fileKeySource reads the key set from a file, again whenever its
// modification time changes.
type fileKeySource struct {
	path    string
	modTime time.Time
}

func (f *fileKeySource) String() string { return f.path }

func (f *fileKeySource) refresh(_ context.Context, store *keyStore, now time.Time) error {
	info, err := os.Stat(f.path)
	if err != nil {
		return fmt.Errorf("key set: %w", err)
	}
	if set, _ := store.current(now); set != nil && info.ModTime().Equal(f.modTime) {
		store.confirm(now)
		return nil
	}
	raw, err := os.ReadFile(f.path)
	if err != nil {
		return fmt.Errorf("key set: %w", err)
	}
	set, err := parseKeySet(raw)
	if err != nil {
		return fmt.Errorf("key set %s: %w", f.path, err)
	}
	store.replace(set, now)
	f.modTime = info.ModTime()
	slog.Info("relay: key set loaded", "source", f.path, "keys", len(set.keys))
	return nil
}

// urlKeySource downloads the key set, sending the last ETag so an unchanged
// set costs a 304, and a bearer token when one is configured.
type urlKeySource struct {
	url    string
	token  string
	client *http.Client
	etag   string
}

func newURLKeySource(rawURL, bearer string) (*urlKeySource, error) {
	if err := checkURL(rawURL); err != nil {
		return nil, err
	}
	return &urlKeySource{url: rawURL, token: bearer, client: noRedirectClient(keysFetchTimeout)}, nil
}

func (u *urlKeySource) String() string { return u.url }

func (u *urlKeySource) refresh(ctx context.Context, store *keyStore, now time.Time) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.url, nil)
	if err != nil {
		return err
	}
	if u.token != "" {
		req.Header.Set("Authorization", "Bearer "+u.token)
	}
	if u.etag != "" {
		req.Header.Set("If-None-Match", u.etag)
	}
	resp, err := u.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }() // not actionable: the body is read below
	switch resp.StatusCode {
	case http.StatusNotModified:
		store.confirm(now)
		return nil
	case http.StatusOK:
	default:
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBody)) // not actionable: drained to reuse the connection
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxKeySetBytes+1))
	if err != nil {
		return err
	}
	if len(raw) > maxKeySetBytes {
		return errors.New("key set is larger than the limit")
	}
	set, err := parseKeySet(raw)
	if err != nil {
		return err
	}
	store.replace(set, now)
	u.etag = resp.Header.Get("ETag")
	slog.Info("relay: key set updated", "source", u.url, "keys", len(set.keys))
	return nil
}
