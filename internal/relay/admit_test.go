package relay

import (
	"context"
	"testing"
	"testing/synctest"

	"github.com/qumo-dev/qumo/internal/auth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSessionGrant(t *testing.T) {
	g := &auth.Grant{}
	tests := map[string]struct {
		ctx  context.Context
		want *auth.Grant
	}{
		"no admission is unchecked": {ctx: context.Background(), want: nil},
		"decided with a grant":      {ctx: withAdmission(context.Background(), decidedAdmission(g)), want: g},
		"decided unchecked":         {ctx: withAdmission(context.Background(), decidedAdmission(nil)), want: nil},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := sessionGrant(tt.ctx)

			require.NoError(t, err)
			assert.Same(t, tt.want, got)
		})
	}
}

func TestSessionGrant_WaitsForPendingAdmission(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a := pendingAdmission()
		ctx := withAdmission(context.Background(), a)
		g := &auth.Grant{}
		var got *auth.Grant
		var err error
		done := make(chan struct{})
		go func() {
			defer close(done)
			got, err = sessionGrant(ctx)
		}()

		synctest.Wait() // sessionGrant is blocked on the pending admission
		select {
		case <-done:
			t.Fatal("sessionGrant returned before the admission was decided")
		default:
		}
		a.decide(g)
		<-done

		require.NoError(t, err)
		assert.Same(t, g, got)
	})
}

func TestSessionGrant_ContextEndsWhilePending(t *testing.T) {
	ctx, cancel := context.WithCancel(withAdmission(context.Background(), pendingAdmission()))
	cancel()

	got, err := sessionGrant(ctx)

	assert.ErrorIs(t, err, context.Canceled)
	assert.Nil(t, got)
}
