package auth

import (
	"encoding/json"
	"net/http"
	"sync"

	"github.com/qumo-dev/qumo/usage"
)

// fakeUsageSink records the batches POSTed to it and answers from a status
// queue: in order, the last repeating; empty means 200.
type fakeUsageSink struct {
	mu       sync.Mutex
	statuses []int
	batches  [][]usage.Record
	gotAuth  string
}

func (f *fakeUsageSink) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gotAuth = r.Header.Get("Authorization")
	var batch []usage.Record
	if err := json.NewDecoder(r.Body).Decode(&batch); err == nil {
		f.batches = append(f.batches, batch)
	}
	status := http.StatusOK
	if len(f.statuses) > 0 {
		status = f.statuses[0]
		if len(f.statuses) > 1 {
			f.statuses = f.statuses[1:]
		}
	}
	w.WriteHeader(status)
}

func (f *fakeUsageSink) received() [][]usage.Record {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]usage.Record(nil), f.batches...)
}
