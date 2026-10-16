// Package integration holds the reusable subscription-gate collector and
// evaluation logic for the OBS/ffmpeg interop tests (PRD #147, M5/M6).
//
// [Evaluate] is a pure gate over an [Observation] and is unit-tested in the
// default test run. The live [Collector] (which subscribes over MoQT and needs
// a relay) lives in collect_test.go behind the "integration" build tag, so it
// is compiled only by `go test -tags=integration` and cannot be imported by
// production code.
package integration

import (
	"fmt"
	"strings"
)

// FrameObs is one observed media frame on the subscriber side, recorded in
// decode (arrival) order.
type FrameObs struct {
	// PTSUS is the presentation timestamp in microseconds, from the MediaFrame
	// envelope. With B-frames this is not monotonic in decode order.
	PTSUS int64
	// Bytes is the codec-specific frame body length (AVCC NALUs / raw AAC).
	Bytes int
	// IsKeyframe is true for video IDR slices. Always false for audio.
	IsKeyframe bool
}

// TrackObs is the per-track observation gathered by a [Collector].
type TrackObs struct {
	Name       string // "video" / "audio"
	Role       string // msf role string
	Codec      string // e.g. "avc1.64001f", "mp4a.40.2"
	InitData   string // Base64 initData from the catalog; "" if absent
	Width      int    // video dimensions; 0 if absent
	Height     int
	Frames     []FrameObs
	GroupCount int
	ReadError  error // non-nil if draining the track failed
}

// KeyframeCount returns the number of keyframes observed on the track.
func (t *TrackObs) KeyframeCount() int {
	n := 0
	for _, f := range t.Frames {
		if f.IsKeyframe {
			n++
		}
	}
	return n
}

// Observation is the structured result a [Collector] returns and the input to
// [Evaluate]. It is assertable independently of a live relay.
type Observation struct {
	CatalogFetched bool                 // a catalog was received and parsed
	CatalogError   error                // non-nil if catalog fetch/parse failed
	Tracks         map[string]*TrackObs // keyed by track Name ("video"/"audio")
	Order          []string             // track Names in announce order
}

// Expectations describes what [Evaluate] considers a well-formed, decodable
// stream. Zero-value fields are not checked.
type Expectations struct {
	WantVideo bool
	WantAudio bool
	// VideoCodecPrefix / AudioCodecPrefix require the catalog codec string to
	// have this prefix (e.g. "avc1.", "mp4a.40.2").
	VideoCodecPrefix string
	AudioCodecPrefix string
	Width            int // 0 = don't check
	Height           int // 0 = don't check
	MinVideoFrames   int
	MinAudioFrames   int
	MinKeyframes     int
	RequireInitData  bool
	// MaxCTSWindowUS bounds how far PTS may regress between consecutive
	// decode-ordered frames (B-frame composition-time reordering). A real
	// stream's PTS is non-decreasing except for dips of up to one CTS window;
	// a larger regression indicates broken timestamps. 0 disables the check.
	MaxCTSWindowUS int64
}

// TrackNames returns the catalog tracks exp wants, for [Collector.WantTracks].
func (exp Expectations) TrackNames() []string {
	var names []string
	if exp.WantVideo {
		names = append(names, "video")
	}
	if exp.WantAudio {
		names = append(names, "audio")
	}
	return names
}

// Verdict is the gate result.
type Verdict struct {
	Pass     bool
	Failures []string
}

// String renders the verdict for test/logging output.
func (v Verdict) String() string {
	if v.Pass {
		return "PASS"
	}
	return "FAIL:\n  - " + strings.Join(v.Failures, "\n  - ")
}

// Evaluate applies the interop gate to obs. It is pure: no I/O, no globals.
func Evaluate(obs *Observation, exp Expectations) Verdict {
	var v Verdict
	if obs == nil {
		return Verdict{Failures: []string{"nil observation"}}
	}
	add := func(format string, args ...any) {
		v.Failures = append(v.Failures, fmt.Sprintf(format, args...))
	}

	if !obs.CatalogFetched || obs.CatalogError != nil {
		add("catalog not fetched: %v", obs.CatalogError)
	}

	if exp.WantVideo {
		vt := obs.Tracks["video"]
		if vt == nil {
			add("video track missing from catalog")
		} else {
			evaluateTrack("video", vt, exp.VideoCodecPrefix, exp.Width, exp.Height,
				exp.MinVideoFrames, exp.MinKeyframes, exp.RequireInitData, exp.MaxCTSWindowUS, add)
		}
	}

	if exp.WantAudio {
		at := obs.Tracks["audio"]
		if at == nil {
			add("audio track missing from catalog")
		} else {
			evaluateTrack("audio", at, exp.AudioCodecPrefix, 0, 0,
				exp.MinAudioFrames, 0, exp.RequireInitData, 0, add)
		}
	}

	v.Pass = len(v.Failures) == 0
	return v
}

func evaluateTrack(name string, t *TrackObs, codecPrefix string, wantW, wantH,
	minFrames, minKeys int, requireInitData bool, maxCTS int64, add func(string, ...any)) {
	if t.ReadError != nil {
		add("%s track read error: %v", name, t.ReadError)
	}
	if codecPrefix != "" && !strings.HasPrefix(t.Codec, codecPrefix) {
		add("%s codec %q does not have prefix %q", name, t.Codec, codecPrefix)
	}
	if wantW != 0 && t.Width != wantW {
		add("%s width %d != expected %d", name, t.Width, wantW)
	}
	if wantH != 0 && t.Height != wantH {
		add("%s height %d != expected %d", name, t.Height, wantH)
	}
	if requireInitData && t.InitData == "" {
		add("%s catalog track carries no initData", name)
	}
	if len(t.Frames) < minFrames {
		add("%s frame count %d < minimum %d", name, len(t.Frames), minFrames)
	}
	if name == "video" && t.KeyframeCount() < minKeys {
		add("video keyframe count %d < minimum %d", t.KeyframeCount(), minKeys)
	}
	if maxCTS > 0 && len(t.Frames) >= 2 {
		// PTS may regress between consecutive decode-ordered frames by at most
		// one composition-time window (B-frame reordering). A larger regression
		// indicates broken timestamps.
		for i := 1; i < len(t.Frames); i++ {
			if t.Frames[i].PTSUS < t.Frames[i-1].PTSUS-maxCTS {
				add("%s PTS regressed beyond CTS window: frame %d pts=%d < frame %d pts=%d (window=%d)",
					name, i, t.Frames[i].PTSUS, i-1, t.Frames[i-1].PTSUS, maxCTS)
				return
			}
		}
	}
}
