package auth

import (
	"net/http"
	"sync"
)

// fakeKeyServer serves a key set with an ETag and records what it was sent.
// A zero value answers 200 with an empty set.
type fakeKeyServer struct {
	mu       sync.Mutex
	body     []byte
	etag     string
	status   int
	gotAuth  string
	gotMatch []string
}

func (f *fakeKeyServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gotAuth = r.Header.Get("Authorization")
	f.gotMatch = append(f.gotMatch, r.Header.Get("If-None-Match"))
	if f.status != 0 {
		w.WriteHeader(f.status)
		return
	}
	if f.etag != "" {
		w.Header().Set("ETag", f.etag)
		if r.Header.Get("If-None-Match") == f.etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	body := f.body
	if body == nil {
		body = []byte(`{"keys":[]}`)
	}
	_, _ = w.Write(body)
}

func (f *fakeKeyServer) set(status int, body []byte, etag string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status, f.body, f.etag = status, body, etag
}
