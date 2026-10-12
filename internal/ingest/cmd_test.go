package ingest

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// A QUIC setting an ingest mode refuses stops it before it listens.
func TestRun_InvalidQUICSetting(t *testing.T) {
	tests := map[string]struct {
		run func() error
	}{
		"rtmp":      {run: func() error { return RunRTMP(nil) }},
		"rtsp":      {run: func() error { return RunRTSP(nil) }},
		"rtsp pull": {run: func() error { return RunRTSPPull([]string{"rtsp://127.0.0.1:1/none"}) }},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Setenv("QUIC_INITIAL_PACKET_SIZE", "900")

			err := tt.run()

			assert.ErrorContains(t, err, "QUIC_INITIAL_PACKET_SIZE")
		})
	}
}
