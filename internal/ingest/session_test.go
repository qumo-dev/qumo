package ingest

import (
	"testing"
	"time"

	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/qumo-dev/qumo/token"
)

func TestNewSession_RejectsInvalidPath(t *testing.T) {
	tests := []struct {
		name string
		path moqt.BroadcastPath
	}{
		{"empty", ""},
		{"missing leading slash", "live/camera"},
		{"relative", "./live"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewSession(moqt.NewTrackMux(0), tt.path)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "invalid broadcast path")
		})
	}
}

func TestNewSession_AcceptsValidPath(t *testing.T) {
	sess, err := NewSession(moqt.NewTrackMux(0), "/live/camera")
	require.NoError(t, err)
	defer sess.Close()
}

// TestSession_OptIn verifies an opt-in is announced beside the broadcast,
// redirecting to it, and withdrawn at its lapse or when the session closes,
// without the broadcast.
func TestSession_OptIn(t *testing.T) {
	mux := moqt.NewTrackMux(0)
	sess, err := NewSession(mux, "/live/cam")
	require.NoError(t, err)

	sess.OptIn(token.OptInABR, time.Now().Add(200*time.Millisecond))
	sess.OptIn(token.OptInHLS, time.Time{})

	assert.True(t, published(mux, "/.abr/live/cam"))
	assert.True(t, published(mux, "/.hls/live/cam"))
	assert.Eventually(t, func() bool { return !published(mux, "/.abr/live/cam") }, 3*time.Second, 20*time.Millisecond,
		"withdrawn at its lapse")
	assert.True(t, published(mux, "/live/cam"), "the broadcast plays on")
	assert.True(t, published(mux, "/.hls/live/cam"), "an opt-in that never lapses stays")

	sess.Close()
	assert.Eventually(t, func() bool { return !published(mux, "/.hls/live/cam") }, 3*time.Second, 20*time.Millisecond,
		"withdrawn with the session")
}
