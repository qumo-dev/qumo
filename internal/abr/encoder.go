package abr

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"sync"

	"github.com/qumo-dev/qumo/internal/cmaf"
)

// EncoderConfig is how renditions are encoded.
type EncoderConfig struct {
	// FFmpeg is the ffmpeg executable: a path, or a name looked up in PATH.
	FFmpeg string
	// Codec is the ffmpeg video encoder, such as "libx264" or "h264_nvenc".
	// It must produce H.264: browsers decode the renditions with the codec
	// string and decoder configuration the encoder outputs.
	Codec string
	// Preset is the encoder's speed preset, passed as -preset; empty passes
	// none. libx264 is also tuned for zero latency.
	Preset string
}

// sourceConfig describes the source video track an encoder decodes.
type sourceConfig struct {
	// Codec is the WebCodecs codec string, such as "avc1.64001f".
	Codec string
	// Width and Height are the coded picture size.
	Width, Height int
	// Description is the AVCDecoderConfigurationRecord.
	Description []byte
}

// output is one rendition an encoder produces.
type output struct {
	Rung          Rung
	Width, Height int
}

// sourceFrame is one frame of the source video track, fed to an encoder.
type sourceFrame struct {
	frameInfo
	// Data is the frame: AVCC length-prefixed NAL units.
	Data []byte
}

// renditionFrame is one frame an encoder outputs.
type renditionFrame struct {
	frameInfo
	// Keyframe reports whether the encoder coded the frame as a keyframe.
	Keyframe bool
	// Data is the frame: AVCC length-prefixed NAL units.
	Data []byte
}

// renditionSink receives what an encoder outputs for each of its renditions,
// indexed as the encoder's outputs are. Its methods are called from one
// goroutine per rendition.
type renditionSink interface {
	// Configure delivers the rendition's AVCDecoderConfigurationRecord, before
	// its first frame and whenever it changes.
	Configure(rendition int, record []byte)
	// WriteFrame delivers a frame of the rendition.
	WriteFrame(rendition int, f renditionFrame)
}

// An encoder transcodes the source video track into renditions with one ffmpeg
// process, which decodes the source once and encodes every rendition from it.
//
// Frames go in as fragmented MP4, one fragment per frame, whose box sizes let
// ffmpeg read each frame the moment it arrives and whose microsecond timescale
// carries LOC timestamps unrounded. Renditions come out as FLV, one pipe each,
// which ffmpeg writes frame by frame without waiting for the next frame (its
// MP4 muxer holds every frame back by one, to learn its duration).
type encoder struct {
	cfg      EncoderConfig
	outputs  []output
	sink     renditionSink
	packager *cmaf.Packager

	cmd     *exec.Cmd
	stdin   io.WriteCloser
	readers []io.ReadCloser
	match   []matcher
	done    sync.WaitGroup

	// Input state, owned by the goroutine calling encode.
	started  bool
	sequence uint32
	last     uint64 // timestamp of the last frame fed
	gap      uint32 // gap between the last two frames fed
}

// defaultFrameGap is the duration stated for a fragment before two frames have
// been seen: a frame at 30 fps. ffmpeg places frames by their decode time, so
// a duration only fills the sample table.
const defaultFrameGap = 1_000_000 / 30

func newEncoder(cfg EncoderConfig, src sourceConfig, outputs []output, sink renditionSink) (*encoder, error) {
	if len(outputs) == 0 {
		return nil, errors.New("abr: encoder needs an output")
	}
	if src.Width <= 0 || src.Width > 0xffff || src.Height <= 0 || src.Height > 0xffff {
		return nil, fmt.Errorf("abr: source picture size %dx%d", src.Width, src.Height)
	}
	p, err := cmaf.NewPackager(cmaf.VideoConfig{
		Codec:       src.Codec,
		Width:       uint16(src.Width),
		Height:      uint16(src.Height),
		Description: src.Description,
	})
	if err != nil {
		return nil, fmt.Errorf("abr: source init segment: %w", err)
	}
	return &encoder{
		cfg:      cfg,
		outputs:  outputs,
		sink:     sink,
		packager: p,
		match:    make([]matcher, len(outputs)),
		gap:      defaultFrameGap,
	}, nil
}

// args returns ffmpeg's arguments. The first output is written to stdout
// (pipe:1), and output i after it to file descriptor 2+i (pipe:3, pipe:4, …),
// which [encoder.start] passes as extra files.
func (e *encoder) args() []string {
	args := []string{
		"-hide_banner", "-loglevel", "error", "-nostats",
		// Read frames as they arrive. -fflags nobuffer must not be added: it
		// discards the packets read while probing, which here is the first
		// GOP.
		"-probesize", "32", "-analyzeduration", "0", "-flags", "low_delay",
		// Keep the source timestamps, so outputs can be matched to the
		// source frames they were encoded from.
		"-copyts",
		"-f", "mp4", "-i", "pipe:0",
	}
	for i, o := range e.outputs {
		rate := strconv.FormatInt(o.Rung.Bitrate, 10)
		args = append(args, "-map", "0:v", "-c:v", e.cfg.Codec)
		if e.cfg.Preset != "" {
			args = append(args, "-preset", e.cfg.Preset)
		}
		if e.cfg.Codec == "libx264" {
			args = append(args, "-tune", "zerolatency")
		}
		args = append(args,
			"-b:v", rate, "-maxrate", rate, "-bufsize", strconv.FormatInt(o.Rung.Bitrate/2, 10),
			"-pix_fmt", "yuv420p",
			// No B-frames, so frames come out in the order they went in, one
			// for one, and a keyframe wherever the source has one, so every
			// source group opens with a keyframe in every rendition.
			"-bf", "0", "-force_key_frames", "source", "-sc_threshold", "0",
			"-fps_mode", "passthrough",
			// Encode in microseconds. The default time base is the inverse
			// of the frame rate ffmpeg guesses, which snaps every timestamp
			// to that grid, while a live source's frames fall off it.
			"-enc_time_base", "1:1000000",
			"-vf", fmt.Sprintf("scale=%d:%d", o.Width, o.Height),
			"-flush_packets", "1",
			"-f", "flv", "pipe:"+strconv.Itoa(outputFD(i)),
		)
	}
	return args
}

// outputFD is the file descriptor ffmpeg writes output i to.
func outputFD(i int) int {
	if i == 0 {
		return 1
	}
	return 2 + i
}

// start starts ffmpeg. It stops when ctx ends, or when [encoder.close] closes
// its input.
func (e *encoder) start(ctx context.Context) error {
	// The executable is the operator's ABR_FFMPEG, and the arguments are built
	// here from the ladder: nothing comes from a subscriber or publisher.
	cmd := exec.CommandContext(ctx, e.cfg.FFmpeg, e.args()...) //nolint:gosec // G204: operator-configured ffmpeg
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	readers := []io.ReadCloser{stdout}
	var writers []*os.File
	for range e.outputs[1:] {
		r, w, err := os.Pipe()
		if err != nil {
			closeAll(readers[1:])
			closeAll(writers)
			return err
		}
		readers = append(readers, r)
		writers = append(writers, w)
	}
	cmd.ExtraFiles = writers
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		closeAll(readers[1:])
		closeAll(writers)
		return fmt.Errorf("abr: start ffmpeg: %w", err)
	}
	// The child holds its own copies of the write ends.
	closeAll(writers)

	e.cmd, e.stdin, e.readers = cmd, stdin, readers
	e.done.Add(1)
	go func() {
		defer e.done.Done()
		logLines(stderr)
	}()
	for i, r := range readers {
		e.done.Add(1)
		go func() {
			defer e.done.Done()
			// read returns only once its stream ends; the end of a stream
			// ffmpeg closed is not worth a warning.
			if err := e.read(i, r); !errors.Is(err, io.EOF) && !errors.Is(err, os.ErrClosed) {
				slog.Warn("abr: read rendition", "rendition", e.outputs[i].Rung.TrackName(), "error", err)
			}
		}()
	}
	return nil
}

func closeAll[T io.Closer](cs []T) {
	for _, c := range cs {
		_ = c.Close() // not actionable: cleaning up after a failed start
	}
}

// logLines logs what ffmpeg writes to stderr, a line at a time.
func logLines(r io.Reader) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		slog.Warn("abr: ffmpeg", "message", sc.Text())
	}
}

// read reads output i's FLV stream and hands its frames to the sink, until
// the stream ends; it returns why, io.EOF at a clean end.
func (e *encoder) read(i int, r io.Reader) error {
	fr := newFLVReader(r)
	for {
		tag, err := fr.Next()
		if err != nil {
			return err
		}
		switch {
		case tag.isSequenceHeader():
			e.sink.Configure(i, tag.payload())
		case tag.isFrame():
			info, ok := e.match[i].match(tag.Timestamp)
			if !ok {
				next, _ := e.match[i].head()
				slog.Warn("abr: encoder output matches no source frame",
					"rendition", e.outputs[i].Rung.TrackName(), "timestamp_ms", tag.Timestamp, "next_source_us", next)
				continue
			}
			e.sink.WriteFrame(i, renditionFrame{frameInfo: info, Keyframe: tag.isKeyframe(), Data: tag.payload()})
		}
	}
}

// encode feeds a source frame to the encoder. It must not be called
// concurrently. A frame whose timestamp does not advance on the last one fed
// is skipped: fragmented MP4 places frames by decode time, which LOC does not
// carry, so the source must not reorder frames (no B-frames).
func (e *encoder) encode(f sourceFrame) error {
	if !e.started {
		if _, err := e.stdin.Write(e.packager.Init()); err != nil {
			return fmt.Errorf("abr: write init segment: %w", err)
		}
		e.started = true
	} else {
		if f.Timestamp <= e.last {
			slog.Warn("abr: source timestamp does not advance; skipping frame (B-frames in the source?)",
				"timestamp", f.Timestamp, "last", e.last)
			return nil
		}
		if gap := f.Timestamp - e.last; gap <= 10*cmaf.Timescale {
			e.gap = uint32(gap)
		}
	}
	e.sequence++
	frag, _, err := e.packager.Fragment(e.sequence,
		[]cmaf.Frame{{Timestamp: f.Timestamp, Sync: f.First, Data: f.Data}}, f.Timestamp, e.gap)
	if err != nil {
		return fmt.Errorf("abr: package frame: %w", err)
	}
	e.last = f.Timestamp
	for i := range e.match {
		e.match[i].push(f.frameInfo)
	}
	if _, err := e.stdin.Write(frag); err != nil {
		return fmt.Errorf("abr: write frame: %w", err)
	}
	return nil
}

// close closes the encoder's input, waits for ffmpeg to flush and exit and for
// every output to be read, and returns how ffmpeg exited.
func (e *encoder) close() error {
	if e.cmd == nil {
		return nil
	}
	_ = e.stdin.Close() // not actionable: ffmpeg sees end of input either way
	// Every pipe is read to its end before Wait, which closes them.
	e.done.Wait()
	closeAll(e.readers[1:])
	return e.cmd.Wait()
}
