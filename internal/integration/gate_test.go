package integration

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// wellFormedObs builds a synthetic Observation that satisfies the default
// Expectations. Individual tests then mutate it to introduce a defect.
func wellFormedObs() *Observation {
	w := int64(1920)
	h := int64(1080)
	return &Observation{
		CatalogFetched: true,
		Order:          []string{"video", "audio"},
		Tracks: map[string]*TrackObs{
			"video": {
				Name:     "video",
				Role:     "video",
				Codec:    "avc1.64001f",
				InitData: "AAAA",
				Width:    int(w),
				Height:   int(h),
				Frames: []FrameObs{
					{PTSUS: 0, IsKeyframe: true, Bytes: 100},
					{PTSUS: 33_333, IsKeyframe: false, Bytes: 50},
					{PTSUS: 66_666, IsKeyframe: false, Bytes: 50},
				},
				GroupCount: 1,
			},
			"audio": {
				Name: "audio", Role: "audio", Codec: "mp4a.40.2", InitData: "AAAA",
				Frames: []FrameObs{{PTSUS: 0, Bytes: 10}, {PTSUS: 21_000, Bytes: 10}},
			},
		},
	}
}

func defaultExp() Expectations {
	return Expectations{
		WantVideo: true, WantAudio: true,
		VideoCodecPrefix: "avc1.", AudioCodecPrefix: "mp4a.40.2",
		Width: 1920, Height: 1080,
		MinVideoFrames: 2, MinAudioFrames: 2, MinKeyframes: 1,
		RequireInitData: true, MaxCTSWindowUS: 100_000,
	}
}

func TestEvaluate_WellFormed(t *testing.T) {
	v := Evaluate(wellFormedObs(), defaultExp())
	assert.True(t, v.Pass, "expected pass; got failures: %v", v.Failures)
}

func TestEvaluate_BFrames_PTSReorderWithinWindow(t *testing.T) {
	// Decode order: I(0), P(33k), B(16k presented earlier), B(50k).
	// PTS regresses 33k→16k (17k < 100k window) — valid B-frame reorder.
	obs := wellFormedObs()
	obs.Tracks["video"].Frames = []FrameObs{
		{PTSUS: 0, IsKeyframe: true},
		{PTSUS: 33_333},
		{PTSUS: 16_000}, // dips back 17k < window
		{PTSUS: 50_000},
	}
	v := Evaluate(obs, defaultExp())
	assert.True(t, v.Pass, "PTS reorder within window should pass; got: %v", v.Failures)
}

func TestEvaluate_BFrames_PTSRegressionBeyondWindow(t *testing.T) {
	obs := wellFormedObs()
	obs.Tracks["video"].Frames = []FrameObs{
		{PTSUS: 0, IsKeyframe: true},
		{PTSUS: 500_000}, // huge forward jump is fine...
		{PTSUS: 100},     // ...but a 499.9k regression exceeds the 100k window.
	}
	v := Evaluate(obs, defaultExp())
	assert.False(t, v.Pass)
	assert.Contains(t, joinFailures(v), "PTS regressed beyond CTS window")
}

func TestEvaluate_NilObservation(t *testing.T) {
	v := Evaluate(nil, defaultExp())
	assert.False(t, v.Pass)
	assert.Contains(t, joinFailures(v), "nil observation")
}

func TestEvaluate_Defects(t *testing.T) {
	t.Run("catalog not fetched", func(t *testing.T) {
		obs := wellFormedObs()
		obs.CatalogFetched = false
		assert.False(t, Evaluate(obs, defaultExp()).Pass)
	})
	t.Run("video track missing", func(t *testing.T) {
		obs := wellFormedObs()
		delete(obs.Tracks, "video")
		v := Evaluate(obs, defaultExp())
		assert.False(t, v.Pass)
		assert.Contains(t, joinFailures(v), "video track missing")
	})
	t.Run("wrong codec prefix", func(t *testing.T) {
		obs := wellFormedObs()
		obs.Tracks["video"].Codec = "avc3.64001f" // Annex-B marker, not avc1
		v := Evaluate(obs, defaultExp())
		assert.False(t, v.Pass)
		assert.Contains(t, joinFailures(v), "does not have prefix")
	})
	t.Run("missing initData", func(t *testing.T) {
		obs := wellFormedObs()
		obs.Tracks["video"].InitData = ""
		v := Evaluate(obs, defaultExp())
		assert.False(t, v.Pass)
		assert.Contains(t, joinFailures(v), "no initData")
	})
	t.Run("wrong dimensions", func(t *testing.T) {
		obs := wellFormedObs()
		obs.Tracks["video"].Width = 1280
		v := Evaluate(obs, defaultExp())
		assert.False(t, v.Pass)
		assert.Contains(t, joinFailures(v), "width 1280 != expected 1920")
	})
	t.Run("too few frames", func(t *testing.T) {
		obs := wellFormedObs()
		obs.Tracks["audio"].Frames = obs.Tracks["audio"].Frames[:1]
		v := Evaluate(obs, defaultExp())
		assert.False(t, v.Pass)
		assert.Contains(t, joinFailures(v), "frame count 1 < minimum 2")
	})
	t.Run("no keyframe", func(t *testing.T) {
		obs := wellFormedObs()
		for i := range obs.Tracks["video"].Frames {
			obs.Tracks["video"].Frames[i].IsKeyframe = false
		}
		v := Evaluate(obs, defaultExp())
		assert.False(t, v.Pass)
		assert.Contains(t, joinFailures(v), "keyframe count 0 < minimum 1")
	})
	t.Run("track read error", func(t *testing.T) {
		obs := wellFormedObs()
		obs.Tracks["video"].ReadError = errSentinel{}
		v := Evaluate(obs, defaultExp())
		assert.False(t, v.Pass)
		assert.Contains(t, joinFailures(v), "video track read error")
	})
}

func TestTrackObs_KeyframeCount(t *testing.T) {
	tr := &TrackObs{Frames: []FrameObs{{IsKeyframe: true}, {}, {IsKeyframe: true}}}
	assert.Equal(t, 2, tr.KeyframeCount())
}

// errSentinel is a minimal error for fixture ReadError fields.
type errSentinel struct{}

func (errSentinel) Error() string { return "sentinel" }

func joinFailures(v Verdict) string {
	out := ""
	for i, f := range v.Failures {
		if i > 0 {
			out += "\n"
		}
		out += f
	}
	return out
}

func TestExpectations_TrackNames(t *testing.T) {
	tests := map[string]struct {
		exp      Expectations
		expected []string
	}{
		"video and audio": {exp: Expectations{WantVideo: true, WantAudio: true}, expected: []string{"video", "audio"}},
		"video only":      {exp: Expectations{WantVideo: true}, expected: []string{"video"}},
		"audio only":      {exp: Expectations{WantAudio: true}, expected: []string{"audio"}},
		"none":            {exp: Expectations{}, expected: nil},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tt.expected, tt.exp.TrackNames())
		})
	}
}
