//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/qumo-dev/gomoqt/moqt"
	"github.com/qumo-dev/gomoqt/msf"
	"github.com/qumo-dev/qumo/internal/ffpub"
	"github.com/stretchr/testify/require"
)

// TestRTSPPlayback_Decodes is the behavioral confirmation that the reshaped
// RTSP frames still form a valid, decodable bitstream — i.e. the access-unit
// aggregation and audio coalescing did not corrupt content while fixing the
// frame shape. It subscribes to an RTSP-published stream, captures video (AVCC)
// and audio (raw AAC) frames, remuxes them to raw H.264 Annex-B and ADTS-AAC
// files, and drives ffmpeg to decode both with -err_detect explode. A clean
// decode (exit 0, no errors, a healthy frame count) proves the NALUs are
// well-formed and the AAC access units are intact.
//
// What this does and does not cover:
//   - It DOES catch content corruption: STAP-A splitting, FU-A reassembly, and
//     mpeg4-generic depacketization must yield valid NALUs / AAC frames (a
//     header byte leak or a truncated AU fails the decode).
//   - It does NOT reproduce the original "broken picture" symptom, which was a
//     player-side (WebCodecs) key/delta mislabeling of same-PTS frames — no Go
//     test can reach the browser decoder. The structural guarantee that every
//     access unit is one frame with a unique PTS (TestRTSPPlayback_FrameIntegrity)
//     is the proxy for that.
func TestRTSPPlayback_Decodes(t *testing.T) {
	if !ffpub.Available() {
		t.Skip("ffmpeg not on PATH")
	}
	mux := moqt.NewTrackMux(0)
	rtspAddr, serveURL := setupRTSPPipeline(t, mux)

	const path = "/live/decode"
	pubCtx, cancelPub := context.WithCancel(context.Background())
	defer cancelPub()
	pub := ffpub.New(ffpub.Config{
		URL:   fmt.Sprintf("rtsp://%s%s", rtspAddr, path),
		Audio: true,
		GOP:   30, Width: 320, Height: 240, Framerate: 30,
	})
	require.NoError(t, pub.Start(pubCtx))
	t.Cleanup(func() { _ = pub.Wait() })
	time.Sleep(2 * time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	sess, err := (&moqt.Dialer{TLSConfig: subscriberTLS(t)}).Dial(ctx, serveURL, moqt.NewTrackMux(0))
	require.NoError(t, err)
	defer sess.CloseWithError(moqt.NoError, "done")

	// Catalog: resolve the tracks and pull audio config for ADTS framing.
	ctr, err := sess.Subscribe(ctx, moqt.BroadcastPath(path), "catalog", nil)
	require.NoError(t, err)
	cgr, err := ctr.AcceptGroup(ctx)
	require.NoError(t, err)
	cbuf := moqt.NewFrame(4096)
	var raw []byte
	for f := range cgr.Frames(cbuf) {
		raw = append(raw, f.Body()...)
	}
	cat, err := msf.ParseCatalog(raw)
	require.NoError(t, err)
	audioCfg := catalogAudioConfig(t, cat)
	videoInit := catalogVideoInitData(t, cat)
	require.NotEmpty(t, videoInit, "video catalog track carries no initData (AVCDecoderConfigurationRecord)")

	vtr, err := sess.Subscribe(ctx, moqt.BroadcastPath(path), "video", nil)
	require.NoError(t, err)
	atr, err := sess.Subscribe(ctx, moqt.BroadcastPath(path), "audio", nil)
	require.NoError(t, err)

	// --- Collect video AVCC and audio raw AAC while the publisher is live. ---
	var videoAVCC []byte
	vbuf := moqt.NewFrame(1 << 20)
	for groups := 0; groups < 4; groups++ {
		gctx, gcancel := context.WithTimeout(ctx, 4*time.Second)
		gr, err := vtr.AcceptGroup(gctx)
		gcancel()
		if err != nil {
			break
		}
		for f := range gr.Frames(vbuf) {
			_, data, derr := decodeMediaFrame(f.Body())
			if derr != nil {
				continue
			}
			videoAVCC = append(videoAVCC, avccToAnnexB(data)...)
		}
	}

	type aacFrame struct{ data []byte }
	var aac []aacFrame
	abuf := moqt.NewFrame(1 << 16)
	for i := 0; i < 30; i++ {
		gctx, gcancel := context.WithTimeout(ctx, 2*time.Second)
		gr, err := atr.AcceptGroup(gctx)
		gcancel()
		if err != nil {
			break
		}
		for f := range gr.Frames(abuf) {
			_, data, derr := decodeMediaFrame(f.Body())
			if derr != nil {
				continue
			}
			aac = append(aac, aacFrame{data: append([]byte(nil), data...)})
		}
	}
	cancelPub()

	require.NotEmpty(t, videoAVCC, "expected to capture video frames")
	require.GreaterOrEqual(t, len(aac), 10, "expected to capture audio frames")

	tmp := t.TempDir()
	videoPath := filepath.Join(tmp, "video.h264")
	audioPath := filepath.Join(tmp, "audio.aac")

	// The RTSP path carries SPS/PPS only in the catalog initData (from the SDP
	// sprop-parameter-sets), not in-band. Prepend them as Annex-B NALUs so the
	// raw .h264 file is self-initializing for ffmpeg's decoder.
	videoFile := avccConfigToAnnexB(videoInit)
	videoFile = append(videoFile, videoAVCC...)
	require.NoError(t, os.WriteFile(videoPath, videoFile, 0o644))

	var audioOut bytes.Buffer
	for _, fr := range aac {
		audioOut.Write(adtsHeader(audioCfg.objectType, audioCfg.freqIdx, audioCfg.chanCfg, 7+len(fr.data)))
		audioOut.Write(fr.data)
	}
	require.NoError(t, os.WriteFile(audioPath, audioOut.Bytes(), 0o644))

	// --- Decode both with the strictest error detection. ---
	t.Run("video decodes cleanly", func(t *testing.T) {
		res, out := ffmpegDecode(t, videoPath)
		// 4 groups at 30 fps ≈ 120 frames; accept a healthy floor.
		require.GreaterOrEqual(t, res.frames, 20,
			"ffmpeg decoded too few video frames (got %d):\n%s", res.frames, out)
		require.NotContains(t, out, "Error while decoding",
			"video decode reported errors:\n%s", out)
	})

	t.Run("audio decodes cleanly", func(t *testing.T) {
		res, out := ffmpegDecode(t, audioPath)
		// ffmpeg 8+ does not report a per-frame count for audio in the progress
		// dump, so assert on decoded output duration instead (~21 ms per AAC
		// frame at 48 kHz; we collected ≥10 frames ≈ ≥200 ms).
		require.GreaterOrEqual(t, res.outTimeUS, int64(200_000),
			"ffmpeg decoded too little audio (out_time_us=%d):\n%s", res.outTimeUS, out)
		require.NotContains(t, out, "Error while decoding",
			"audio decode reported errors:\n%s", out)
	})
}

// catalogAudioConfig pulls the AAC object type, frequency index, and channel
// config needed to build ADTS headers from the parsed MSF catalog.
type adtsConfig struct {
	objectType int
	freqIdx    int
	chanCfg    int
}

func catalogAudioConfig(t *testing.T, cat msf.Catalog) adtsConfig {
	t.Helper()
	for i := range cat.Tracks {
		tr := &cat.Tracks[i]
		if string(tr.Role) != string(msf.RoleAudio) {
			continue
		}
		cfg := adtsConfig{objectType: 2} // AAC-LC; the codec string is "mp4a.40.2".
		if tr.SampleRate != nil {
			cfg.freqIdx = aacFreqIndex(int(*tr.SampleRate))
		}
		if tr.ChannelConfig != "" {
			if c, err := strconv.Atoi(tr.ChannelConfig); err == nil {
				cfg.chanCfg = c
			}
		}
		return cfg
	}
	t.Fatal("no audio track in catalog")
	return adtsConfig{}
}

// avccToAnnexB converts one AVCC sample (one or more 4-byte-length-prefixed
// NALUs) to Annex-B: each length prefix becomes the 0x00000001 start code. This
// is what ffmpeg expects in a raw .h264 file.
func avccToAnnexB(avcc []byte) []byte {
	var out []byte
	for off := 0; off+4 <= len(avcc); {
		n := int(binary.BigEndian.Uint32(avcc[off:]))
		off += 4
		if n == 0 || off+n > len(avcc) {
			break
		}
		out = append(out, 0x00, 0x00, 0x00, 0x01)
		out = append(out, avcc[off:off+n]...)
		off += n
	}
	return out
}

// aacFreqIndex maps an AAC sample rate in Hz to its MPEG-4 sampling frequency
// index. Returns 0 (96000) for unknown rates, which is wrong but never exercised
// by the test vectors (48 kHz from ffmpeg's sine source).
func aacFreqIndex(hz int) int {
	switch hz {
	case 96000:
		return 0
	case 88200:
		return 1
	case 64000:
		return 2
	case 48000:
		return 3
	case 44100:
		return 4
	case 32000:
		return 5
	case 24000:
		return 6
	case 22050:
		return 7
	case 16000:
		return 8
	case 12000:
		return 9
	case 11025:
		return 10
	case 8000:
		return 11
	case 7350:
		return 12
	}
	return 0
}

// adtsHeader builds a 7-byte (CRC-less) ADTS header for one AAC frame.
func adtsHeader(objectType, freqIdx, chanCfg, frameLen int) []byte {
	profile := objectType - 1 // AAC-LC object type 2 -> AOT profile 1
	h := make([]byte, 7)
	h[0] = 0xFF // syncword high 8 bits
	h[1] = 0xF1 // sync low 4 + MPEG-4 + layer 0 + no CRC
	h[2] = byte(profile<<6|freqIdx<<2|(chanCfg>>2)) & 0xFF
	h[3] = byte((chanCfg&0x3)<<6) | byte((frameLen>>11)&0x3)
	h[4] = byte((frameLen >> 3) & 0xFF)
	h[5] = byte((frameLen&0x7)<<5) | 0x1F // buffer fullness high (VBR)
	h[6] = 0xFC                           // buffer fullness low + 0 raw blocks
	return h
}

// catalogVideoInitData returns the Base64-decoded AVCDecoderConfigurationRecord
// from the video track in the parsed MSF catalog — the SPS/PPS source needed to
// self-initialize a raw Annex-B file.
func catalogVideoInitData(t *testing.T, cat msf.Catalog) []byte {
	t.Helper()
	for i := range cat.Tracks {
		tr := &cat.Tracks[i]
		if string(tr.Role) != string(msf.RoleVideo) {
			continue
		}
		if tr.InitRef == "" {
			return nil
		}
		var data string
		for i := range cat.InitDataList {
			if cat.InitDataList[i].ID == tr.InitRef {
				data = cat.InitDataList[i].Data
				break
			}
		}
		if data == "" {
			return nil
		}
		b, err := base64.StdEncoding.DecodeString(data)
		require.NoError(t, err, "decoding video initData")
		return b
	}
	return nil
}

// avccConfigToAnnexB extracts the SPS/PPS NALUs from an AVCDecoderConfiguration
// Record (ISO 14496-15) and emits them as Annex-B (start-code-prefixed). Used to
// prepend parameter sets to a raw picture-only .h264 file so ffmpeg can init the
// decoder.
func avccConfigToAnnexB(cfg []byte) []byte {
	if len(cfg) < 7 {
		return nil
	}
	var out []byte
	off := 5 // version(1) + profile(1) + compat(1) + level(1) + lengthSize(1)
	if off >= len(cfg) {
		return nil
	}
	numSPS := int(cfg[off] & 0x1F)
	off++
	for range numSPS {
		if off+2 > len(cfg) {
			return out
		}
		n := int(binary.BigEndian.Uint16(cfg[off:]))
		off += 2
		if off+n > len(cfg) {
			return out
		}
		out = append(out, 0x00, 0x00, 0x00, 0x01)
		out = append(out, cfg[off:off+n]...)
		off += n
	}
	if off >= len(cfg) {
		return out
	}
	numPPS := int(cfg[off])
	off++
	for range numPPS {
		if off+2 > len(cfg) {
			return out
		}
		n := int(binary.BigEndian.Uint16(cfg[off:]))
		off += 2
		if off+n > len(cfg) {
			return out
		}
		out = append(out, 0x00, 0x00, 0x00, 0x01)
		out = append(out, cfg[off:off+n]...)
		off += n
	}
	return out
}

// decodeResult is the parsed outcome of an ffmpeg decode-to-null run.
type decodeResult struct {
	frames    int
	outTimeUS int64
}

// ffmpegDecode runs `ffmpeg -err_detect explode -i path -f null -` and returns
// the decoded-frame count / output duration plus the combined output. Any decode
// error makes ffmpeg exit non-zero, which fails the test. Counts come from
// `-progress pipe:1` (ffmpeg 8+ no longer prints the legacy `frame=` line on
// stderr by default).
func ffmpegDecode(t *testing.T, path string) (decodeResult, string) {
	t.Helper()
	cmd := exec.Command("ffmpeg", "-hide_banner", "-err_detect", "explode",
		"-progress", "pipe:1", "-i", path, "-f", "null", "-")
	out, err := cmd.CombinedOutput()
	outs := string(out)
	if err != nil {
		t.Fatalf("ffmpeg decode of %s failed: %v\n%s", path, err, outs)
	}
	return parseFFmpegProgress(outs), outs
}

// parseFFmpegProgress reads the last frame= and out_time_us= values from a
// `-progress pipe:1` dump (the final values are the totals).
func parseFFmpegProgress(out string) decodeResult {
	var res decodeResult
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if v, ok := strings.CutPrefix(line, "frame="); ok {
			if x, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
				res.frames = x
			}
		}
		if v, ok := strings.CutPrefix(line, "out_time_us="); ok {
			if x, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil {
				res.outTimeUS = x
			}
		}
	}
	return res
}
