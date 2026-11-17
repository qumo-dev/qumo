package ingest

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/qumo-dev/qumo/internal/rtmp"
	"github.com/qumo-dev/qumo/token"
)

// startRTMPServer stands up an RTMPServer checking publishes with authorize
// (nil: none) and opting streams into optIns, on an ephemeral loopback port,
// and returns its TrackMux and the bound address once it is listening. It is
// torn down on test cleanup.
func startRTMPServer(t *testing.T, authorize PublishAuthorizer, optIns ...token.OptIn) (*moqt.TrackMux, net.Addr) {
	t.Helper()
	mux := moqt.NewTrackMux(0)
	server := NewRTMPServer(RTMPConfig{Addr: "127.0.0.1:0", TrackMux: mux, Authorize: authorize, OptIns: optIns})
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- server.ListenAndServe(ctx) }()
	t.Cleanup(func() {
		cancel()
		_ = server.Shutdown(context.Background())
		<-errCh
	})
	var addr net.Addr
	require.Eventually(t, func() bool {
		addr = server.Addr()
		return addr != nil
	}, 2*time.Second, 10*time.Millisecond, "RTMP server never started listening")
	return mux, addr
}

// publishRTMP opens a publish of app/streamKey on the server at addr. The
// connection is closed on test cleanup.
func publishRTMP(t *testing.T, addr net.Addr, app, streamKey string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, err := rtmp.Dial(ctx, addr.String())
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	_, err = conn.OpenStream(app, streamKey)
	require.NoError(t, err)
}

// published reports whether mux serves the broadcast at path: it holds an
// announcement for it.
func published(mux *moqt.TrackMux, path moqt.BroadcastPath) bool {
	announcement, _ := mux.TrackHandler(path)
	return announcement != nil
}

func TestRTMPServer_Authorize(t *testing.T) {
	tests := map[string]struct {
		err           error
		wantPublished bool
	}{
		"accepted": {wantPublished: true},
		"refused":  {err: errors.New("no grant")},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			authorize := &fakePublishAuthorizer{err: tt.err}
			mux, addr := startRTMPServer(t, authorize.authorize)

			publishRTMP(t, addr, "live", "cam?jwt=tok")

			require.Eventually(t, func() bool { return len(authorize.questions()) == 1 }, 3*time.Second, 10*time.Millisecond)
			assert.Equal(t, []publishAsk{{credential: "tok", path: "/live/cam"}}, authorize.questions(),
				"the credential from the stream key's query, the path without it")
			if tt.wantPublished {
				assert.Eventually(t, func() bool { return published(mux, "/live/cam") }, 3*time.Second, 10*time.Millisecond)
				return
			}
			assert.Never(t, func() bool { return published(mux, "/live/cam") }, 300*time.Millisecond, 50*time.Millisecond)
		})
	}
}

// TestRTMPServer_Authorize_CutsAtLapse verifies a publish is cut, and its
// broadcast withdrawn, when its permission lapses.
func TestRTMPServer_Authorize_CutsAtLapse(t *testing.T) {
	authorize := &fakePublishAuthorizer{lapses: time.Now().Add(300 * time.Millisecond)}
	mux, addr := startRTMPServer(t, authorize.authorize)

	publishRTMP(t, addr, "live", "cam?jwt=tok")

	require.Eventually(t, func() bool { return published(mux, "/live/cam") }, 3*time.Second, 10*time.Millisecond)
	assert.Eventually(t, func() bool { return !published(mux, "/live/cam") }, 3*time.Second, 20*time.Millisecond)
}

// TestRTMPServer_OptIns verifies a stream is opted into each egress the
// server offers that one of its credentials may publish the opt-in of: a
// second ?jwt= on the stream key, which the broadcast's own does not grant.
func TestRTMPServer_OptIns(t *testing.T) {
	errNoGrant := errors.New("no grant")
	// tok publishes the broadcast; abr publishes its ABR opt-in alone.
	grants := map[publishAsk]bool{
		{credential: "tok", path: "/live/cam"}:      true,
		{credential: "abr", path: "/.abr/live/cam"}: true,
	}
	tests := map[string]struct {
		authorize bool
		streamKey string
		wantABR   bool
	}{
		"a second credential that may publish the opt-in": {authorize: true, streamKey: "cam?jwt=tok&jwt=abr", wantABR: true},
		"the broadcast's credential alone":                {authorize: true, streamKey: "cam?jwt=tok"},
		"auth off opts every stream in":                   {streamKey: "cam", wantABR: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var authorize PublishAuthorizer
			if tt.authorize {
				fake := &fakePublishAuthorizer{answer: func(ask publishAsk) (time.Time, error) {
					if grants[ask] {
						return time.Time{}, nil
					}
					return time.Time{}, errNoGrant
				}}
				authorize = fake.authorize
			}
			mux, addr := startRTMPServer(t, authorize, token.OptInABR)

			publishRTMP(t, addr, "live", tt.streamKey)

			require.Eventually(t, func() bool { return published(mux, "/live/cam") }, 3*time.Second, 10*time.Millisecond)
			if tt.wantABR {
				assert.Eventually(t, func() bool { return published(mux, "/.abr/live/cam") }, 3*time.Second, 10*time.Millisecond)
			} else {
				assert.Never(t, func() bool { return published(mux, "/.abr/live/cam") }, 300*time.Millisecond, 50*time.Millisecond)
			}
			assert.False(t, published(mux, "/.hls/live/cam"), "an egress the server does not offer")
		})
	}
}

// TestRTMPServer_RefusesOptInPaths verifies a push can't name an opt-in's
// path itself: the server announces opt-ins there.
func TestRTMPServer_RefusesOptInPaths(t *testing.T) {
	mux, addr := startRTMPServer(t, nil, token.OptInABR)

	publishRTMP(t, addr, ".abr", "live/cam")

	assert.Never(t, func() bool { return published(mux, "/.abr/live/cam") }, 300*time.Millisecond, 50*time.Millisecond)
}
