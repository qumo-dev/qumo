package hls

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBroadcastGate_Acquire(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		gate := newBroadcastGate()
		release, err := gate.acquire(t.Context(), "/live/cam1")
		require.NoError(t, err)

		// A second handler for the same broadcast waits for the first.
		acquired := make(chan struct{})
		go func() {
			release2, err := gate.acquire(context.Background(), "/live/cam1")
			assert.NoError(t, err)
			close(acquired)
			release2()
		}()
		synctest.Wait()
		select {
		case <-acquired:
			t.Error("the broadcast was acquired twice")
		default:
		}

		// Another broadcast is not held up.
		releaseOther, err := gate.acquire(t.Context(), "/live/cam2")
		require.NoError(t, err)
		releaseOther()

		release()
		synctest.Wait()
		select {
		case <-acquired:
		default:
			t.Error("the waiting handler did not get the broadcast once it was released")
		}
	})
}

func TestBroadcastGate_Acquire_ContextEnds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		gate := newBroadcastGate()
		release, err := gate.acquire(t.Context(), "/live/cam1")
		require.NoError(t, err)
		defer release()

		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		_, err = gate.acquire(ctx, "/live/cam1")

		assert.ErrorIs(t, err, context.DeadlineExceeded)
	})
}
