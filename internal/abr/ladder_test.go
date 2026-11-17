package abr

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseLadder(t *testing.T) {
	tests := map[string]struct {
		in      string
		want    []Rung
		wantErr bool
	}{
		"default": {in: DefaultLadder, want: []Rung{{720, 2_500_000}, {360, 800_000}}},
		"sorted tallest first": {
			in:   "360:800, 1080:6000,720:2500",
			want: []Rung{{1080, 6_000_000}, {720, 2_500_000}, {360, 800_000}},
		},
		"trailing comma":    {in: "480:1200,", want: []Rung{{480, 1_200_000}}},
		"empty":             {in: " ", wantErr: true},
		"no bitrate":        {in: "720", wantErr: true},
		"bad height":        {in: "x:100", wantErr: true},
		"zero bitrate":      {in: "720:0", wantErr: true},
		"duplicate height":  {in: "720:2500,720:2000", wantErr: true},
		"negative bitrate":  {in: "720:-1", wantErr: true},
		"height too small":  {in: "1:100", wantErr: true},
		"non-numeric kbps":  {in: "720:fast", wantErr: true},
		"missing separator": {in: "720-2500", wantErr: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := ParseLadder(tt.in)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestRungSize(t *testing.T) {
	tests := map[string]struct {
		rung         Rung
		srcW, srcH   int
		wantW, wantH int
	}{
		"16:9 to 720p":       {Rung{Height: 720}, 1920, 1080, 1280, 720},
		"16:9 to 360p":       {Rung{Height: 360}, 1920, 1080, 640, 360},
		"16:9 to 240p, even": {Rung{Height: 240}, 1280, 720, 426, 240},
		"4:3 to 480p":        {Rung{Height: 480}, 1440, 1080, 640, 480},
		"odd height rounded": {Rung{Height: 361}, 1920, 1080, 640, 360},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			w, h := tt.rung.Size(tt.srcW, tt.srcH)
			assert.Equal(t, tt.wantW, w)
			assert.Equal(t, tt.wantH, h)
			assert.Zero(t, w%2)
			assert.Zero(t, h%2)
		})
	}
}

func TestBelow(t *testing.T) {
	ladder := []Rung{{1080, 6_000_000}, {720, 2_500_000}, {360, 800_000}}
	assert.Equal(t, []Rung{{360, 800_000}}, below(ladder, 720), "a rung as tall as the source is not encoded")
	assert.Equal(t, ladder, below(ladder, 2160))
	assert.Empty(t, below(ladder, 360))
}

func TestRungTrackName(t *testing.T) {
	assert.Equal(t, "video-720p", Rung{Height: 720}.TrackName())
}
