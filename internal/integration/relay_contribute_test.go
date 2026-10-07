//go:build integration

// Black-box tests of contributions (qumo-dev/qumo#485) on a real QUIC/MOQT
// relay: a session offers one track of a broadcast another session announces,
// and a subscriber of that track is served by the contributor.
package integration

import (
	"context"
	"testing"
	"time"

	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/qumo-dev/qumo/internal/auth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// contributeOver dials url and asks to contribute the track name of path.
// Once the relay subscribes, the track sends a one-frame group holding body
// every 50 ms. The returned channel yields the contribution's end: Contribute's
// error when the relay refused it, nil when a subscription it served ended.
func contributeOver(t *testing.T, url string, path moqt.BroadcastPath, name moqt.TrackName, body string) <-chan error {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	sess := dialOver(t, url, nil, moqt.NewTrackMux(0))
	done := make(chan error, 1)
	go func() {
		tw, err := sess.Contribute(ctx, path, name, nil)
		if err != nil {
			done <- err
			return
		}
		defer tw.Close()
		for {
			gw, err := tw.OpenGroup(ctx)
			if err != nil {
				done <- nil
				return
			}
			fr := moqt.NewFrame(len(body))
			_, _ = fr.Write([]byte(body))
			_ = gw.WriteFrame(fr)
			_ = gw.Close()
			select {
			case <-tw.Context().Done():
				done <- nil
				return
			case <-time.After(50 * time.Millisecond):
			}
		}
	}()
	return done
}

// firstFrame subscribes to a track over a new session to url and returns the
// body of the first frame it receives.
func firstFrame(t *testing.T, url string, path moqt.BroadcastPath, name moqt.TrackName) (string, error) {
	t.Helper()
	sess := dialOver(t, url, nil, moqt.NewTrackMux(0))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tr, err := sess.Subscribe(ctx, path, name, nil)
	if err != nil {
		return "", err
	}
	defer tr.Close()
	gr, err := tr.AcceptGroup(ctx)
	if err != nil {
		return "", err
	}
	fr := moqt.NewFrame(64)
	if err := gr.ReadFrame(fr); err != nil {
		return "", err
	}
	return string(fr.Body()), nil
}

// refused waits for a contribution to end and returns why: an error when the
// relay reset it before subscribing, nil when a subscription it served ended.
func refused(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("the contribution was neither refused nor ended")
		return nil
	}
}

func TestRelay_Contribute(t *testing.T) {
	addr, srv := startAuthRelay(t, authOff, nil)
	url := "https://" + addr + "/"
	// The room's announcer serves any track itself with the body "frame".
	publishOver(t, srv, url, "/room/1/chat")

	t.Run("a contributed track is served by its contributor", func(t *testing.T) {
		contributeOver(t, url, "/room/1/chat", "alice", "from alice")

		var body string
		require.Eventually(t, func() bool {
			got, err := firstFrame(t, url, "/room/1/chat", "alice")
			body = got
			return err == nil && got == "from alice"
		}, 5*time.Second, 50*time.Millisecond, "last body: %q", body)
	})
	t.Run("every other track still comes from the announcer", func(t *testing.T) {
		body, err := firstFrame(t, url, "/room/1/chat", "video")

		require.NoError(t, err)
		assert.Equal(t, "frame", body)
	})
	t.Run("a path nobody announced takes no contribution", func(t *testing.T) {
		done := contributeOver(t, url, "/room/2/chat", "alice", "from alice")

		assert.Error(t, refused(t, done))
	})
	t.Run("a broader route is not enough", func(t *testing.T) {
		done := contributeOver(t, url, "/room/1/chat/thread", "alice", "from alice")

		assert.Error(t, refused(t, done))
	})
}

func TestRelay_Contribute_NewestReplaces(t *testing.T) {
	addr, srv := startAuthRelay(t, authOff, nil)
	url := "https://" + addr + "/"
	publishOver(t, srv, url, "/room/1/chat")
	first := contributeOver(t, url, "/room/1/chat", "alice", "first")
	require.Eventually(t, func() bool {
		got, err := firstFrame(t, url, "/room/1/chat", "alice")
		return err == nil && got == "first"
	}, 5*time.Second, 50*time.Millisecond)

	contributeOver(t, url, "/room/1/chat", "alice", "second")

	// The earlier offer was being served, so it ends without an error: only
	// an offer the relay never subscribed to reports a refusal.
	_ = refused(t, first)
	require.Eventually(t, func() bool {
		got, err := firstFrame(t, url, "/room/1/chat", "alice")
		return err == nil && got == "second"
	}, 5*time.Second, 50*time.Millisecond)
}

func TestRelay_Contribute_Grant(t *testing.T) {
	// The announcer may publish the whole room; the contributor only what
	// lies under its own track name.
	owner := testGrant(t, "room/**", "room/**", time.Time{}, 0)
	alice := testGrant(t, "room/1/chat/alice/**", "room/**", time.Time{}, 0)
	authorize := func(_ context.Context, req auth.Request) (*auth.Grant, error) {
		if req.Query == "jwt=alice" {
			return alice, nil
		}
		return owner, nil
	}
	addr, srv := startAuthRelay(t, authorize, nil)
	publishOver(t, srv, "https://"+addr+"/?jwt=owner", "/room/1/chat")
	aliceURL := "https://" + addr + "/?jwt=alice"

	t.Run("its own track name is covered", func(t *testing.T) {
		contributeOver(t, aliceURL, "/room/1/chat", "alice", "from alice")

		require.Eventually(t, func() bool {
			got, err := firstFrame(t, aliceURL, "/room/1/chat", "alice")
			return err == nil && got == "from alice"
		}, 5*time.Second, 50*time.Millisecond)
	})
	t.Run("another track name is not", func(t *testing.T) {
		done := contributeOver(t, aliceURL, "/room/1/chat", "bob", "from alice")

		assert.Error(t, refused(t, done))
	})
}
