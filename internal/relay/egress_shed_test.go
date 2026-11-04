package relay

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewEgressShed(t *testing.T) {
	assert.Nil(t, newEgressShed(0), "0 is off")

	e := newEgressShed(1000)
	require.NotNil(t, e)
	assert.Equal(t, uint64(1000), e.limit)
	assert.Equal(t, uint64(800), e.resume)
	assert.False(t, e.shedding(), "a new shedder is not shedding")
}

func TestEgressShed_Update(t *testing.T) {
	// Each case starts from the given state, reads heap once, and checks
	// the state after. limit 1000, resume 800.
	tests := map[string]struct {
		active bool
		heap   uint64
		want   bool
	}{
		"below limit stays off":           {active: false, heap: 900, want: false},
		"at limit stays off":              {active: false, heap: 1000, want: false},
		"past limit starts":               {active: false, heap: 1001, want: true},
		"between resume and limit holds":  {active: true, heap: 900, want: true},
		"at resume holds":                 {active: true, heap: 800, want: true},
		"below resume stops":              {active: true, heap: 799, want: false},
		"far past limit keeps shedding":   {active: true, heap: 5000, want: true},
		"zero heap while shedding stops":  {active: true, heap: 0, want: false},
		"zero heap while off stays off":   {active: false, heap: 0, want: false},
		"max heap while off starts":       {active: false, heap: ^uint64(0), want: true},
		"between resume and limit is off": {active: false, heap: 900, want: false},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			heap := tt.heap
			e := &egressShed{limit: 1000, resume: 800, read: func() uint64 { return heap }}
			e.active.Store(tt.active)
			e.update()
			assert.Equal(t, tt.want, e.shedding())
		})
	}
}

func TestEgressShed_NilSafe(t *testing.T) {
	var e *egressShed
	assert.False(t, e.shedding())
	// run on nil returns at once rather than ticking forever.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	e.run(ctx)
	assert.NoError(t, ctx.Err(), "run(nil) returned before the context ended")

	var s *statsSampler
	assert.False(t, s.shedding(), "a nil sampler never sheds")
	assert.False(t, (&statsSampler{}).shedding(), "a sampler without a shedder never sheds")
}

func TestReadHeapObjects(t *testing.T) {
	assert.Greater(t, readHeapObjects(), uint64(0), "a running test has heap objects")
}
