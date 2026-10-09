package auth

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	// keysRefresh is how often the key set is refreshed: a new key works,
	// and a withdrawn one stops, within one refresh. With the 30 s re-check
	// of live sessions, a withdrawn key's sessions end within about a minute
	// at worst, half that on average.
	keysRefresh = 30 * time.Second
	// keysRefreshJitter spreads each refresh by up to ±10%, so relays
	// restarted together (a deploy) don't poll the key-set server in step.
	keysRefreshJitter = keysRefresh / 10
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

// replace makes set the current key set. A set that pauses keys with the
// deprecated "publish": false is warned about when it is first loaded and
// whenever those keys change, not on every refresh.
func (s *keyStore) replace(set *keySet, now time.Time) {
	s.mu.Lock()
	prev := s.set
	s.set, s.lastSuccess = set, now
	s.mu.Unlock()
	if len(set.deprecatedPublish) > 0 &&
		(prev == nil || !slices.Equal(prev.deprecatedPublish, set.deprecatedPublish)) {
		slog.Warn(`relay: key set pauses keys with the deprecated "publish": false; write "pause": ["publish"]`,
			"kids", set.deprecatedPublish)
	}
}

func (s *keyStore) confirm(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastSuccess = now
}

// keySource fills a keyStore.
type keySource interface {
	// start loads what the source can give without waiting, before the
	// relay takes sessions. An error stops the relay from starting.
	start(store *keyStore, now time.Time) error
	// refresh brings the store up to date once.
	refresh(ctx context.Context, store *keyStore, now time.Time) error
	// String names the source in logs.
	String() string
}

// nextRefresh returns the wait before the next refresh: keysRefresh, give or
// take up to keysRefreshJitter. The jitter comes from crypto/rand, which at
// one draw per refresh costs nothing; if it fails, the wait is keysRefresh.
func nextRefresh() time.Duration {
	n, err := rand.Int(rand.Reader, big.NewInt(int64(2*keysRefreshJitter)+1))
	if err != nil {
		return keysRefresh
	}
	return keysRefresh - keysRefreshJitter + time.Duration(n.Int64())
}

// runKeySource refreshes store from src now and then about every keysRefresh
// (nextRefresh) until ctx ends. The first failure after a success is logged as an error, repeats
// quietly, and the recovery as info: the last set stays in use throughout.
func runKeySource(ctx context.Context, src keySource, store *keyStore, now func() time.Time) {
	timer := time.NewTimer(nextRefresh())
	defer timer.Stop()
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
		case <-timer.C:
			timer.Reset(nextRefresh())
		}
	}
}

// keySourceFor returns the key source a QUMO_AUTH_KEYS value names, by its
// form: an https URL (or http on a loopback host) is downloaded; a file://
// URL or anything without a scheme, a Windows path included, is a file. Any
// other scheme is refused rather than guessed at.
//
// cache is where a downloaded key set is kept between runs
// (QUMO_AUTH_KEYS_CACHE); it applies to a URL only.
func keySourceFor(value, bearer, cache string) (keySource, error) {
	if value == "" {
		return nil, errors.New("not set")
	}
	scheme, rest, hasScheme := strings.Cut(value, "://")
	isURL := hasScheme && (strings.EqualFold(scheme, "https") || strings.EqualFold(scheme, "http"))
	if cache != "" && !isURL {
		return nil, errors.New("QUMO_AUTH_KEYS_CACHE keeps a downloaded key set; it has no use with a key set file")
	}
	if !hasScheme {
		return &fileKeySource{path: value}, nil
	}
	switch strings.ToLower(scheme) {
	case "https", "http":
		src, err := newURLKeySource(value, bearer)
		if err != nil {
			return nil, err
		}
		src.cache = cache
		return src, nil
	case "file":
		u, err := url.Parse(value)
		if err != nil {
			return nil, err
		}
		if u.Host != "" && u.Host != "localhost" {
			return nil, fmt.Errorf("a file:// URL names a local file, not one on %q", u.Host)
		}
		path := u.Path
		if path == "" {
			path = rest
		}
		// file:///C:/keys.json has the path /C:/keys.json; drop the slash
		// before a drive letter.
		if len(path) >= 3 && path[0] == '/' && path[2] == ':' {
			path = path[1:]
		}
		return &fileKeySource{path: path}, nil
	default:
		return nil, fmt.Errorf("want an https URL or a file path, got scheme %q", scheme)
	}
}

// fileKeySource reads the key set from a file, again whenever its
// modification time changes.
type fileKeySource struct {
	path    string
	modTime time.Time
}

var (
	_ keySource = (*fileKeySource)(nil)
	_ keySource = (*urlKeySource)(nil)
)

func (f *fileKeySource) String() string { return f.path }

// start reads the file. A file that can't be read or parsed stops the relay
// from starting: it would otherwise run and refuse every session.
func (f *fileKeySource) start(store *keyStore, now time.Time) error {
	return f.refresh(context.Background(), store, now)
}

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
	// cache is a file the last downloaded key set is kept in, so a relay
	// that restarts while the URL can't be reached still has its keys. Its
	// modification time is when the set was last confirmed, so the
	// fail-static limit holds across restarts. Empty keeps none.
	cache string
}

// start loads the cached key set, if there is one. It never fails: a missing
// or unreadable cache only means the relay waits for its first download.
func (u *urlKeySource) start(store *keyStore, now time.Time) error {
	if u.cache == "" {
		return nil
	}
	info, err := os.Stat(u.cache)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			slog.Warn("relay: key set cache can't be read", "cache", u.cache, "error", err)
		}
		return nil
	}
	raw, err := os.ReadFile(u.cache)
	if err != nil {
		slog.Warn("relay: key set cache can't be read", "cache", u.cache, "error", err)
		return nil
	}
	set, err := parseKeySet(raw)
	if err != nil {
		slog.Warn("relay: key set cache is not a key set; ignored", "cache", u.cache, "error", err)
		return nil
	}
	// The cache is as old as its last confirmation, not as new as now.
	confirmed := info.ModTime()
	if confirmed.After(now) {
		confirmed = now
	}
	store.replace(set, confirmed)
	slog.Info("relay: key set loaded from cache", "cache", u.cache, "keys", len(set.keys),
		"age", now.Sub(confirmed).Round(time.Second))
	return nil
}

// saveCache writes a downloaded key set to the cache, through a temporary
// file so a crash can't leave half of one. A failure is logged, not returned:
// the relay has the set in memory either way.
func (u *urlKeySource) saveCache(raw []byte) {
	if u.cache == "" {
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(u.cache), filepath.Base(u.cache)+".*.tmp")
	if err != nil {
		slog.Warn("relay: key set cache can't be written", "cache", u.cache, "error", err)
		return
	}
	_, werr := tmp.Write(raw)
	cerr := tmp.Close()
	if err := errors.Join(werr, cerr); err != nil {
		slog.Warn("relay: key set cache can't be written", "cache", u.cache, "error", err)
		_ = os.Remove(tmp.Name()) // not actionable: a leftover temp file
		return
	}
	if err := os.Rename(tmp.Name(), u.cache); err != nil {
		slog.Warn("relay: key set cache can't be written", "cache", u.cache, "error", err)
		_ = os.Remove(tmp.Name()) // not actionable: a leftover temp file
	}
}

// touchCache marks the cached key set as confirmed at now.
func (u *urlKeySource) touchCache(now time.Time) {
	if u.cache == "" {
		return
	}
	if err := os.Chtimes(u.cache, now, now); err != nil && !errors.Is(err, os.ErrNotExist) {
		slog.Debug("relay: key set cache can't be touched", "cache", u.cache, "error", err)
	}
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
		u.touchCache(now)
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
	u.saveCache(raw)
	slog.Info("relay: key set updated", "source", u.url, "keys", len(set.keys))
	return nil
}
