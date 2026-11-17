package abr

import (
	"errors"
	"io"
	"os"
	"testing"
)

// fakeFFmpegDir names the environment variable that makes the test binary
// stand in for ffmpeg: started as an encoder's ffmpeg, it reads its input to
// the end, as ffmpeg does, then writes fakeFFmpegMarker in the directory the
// variable names, so a test can tell that the encoder closed its input and
// waited for it.
const (
	fakeFFmpegDir    = "QUMO_ABR_FAKE_FFMPEG_DIR"
	fakeFFmpegMarker = "closed"
)

func TestMain(m *testing.M) {
	if dir := os.Getenv(fakeFFmpegDir); dir != "" {
		os.Exit(fakeFFmpeg(dir))
	}
	os.Exit(m.Run())
}

func fakeFFmpeg(dir string) int {
	if _, err := io.Copy(io.Discard, os.Stdin); err != nil {
		return 1
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return 1
	}
	if err := errors.Join(root.WriteFile(fakeFFmpegMarker, nil, 0o600), root.Close()); err != nil {
		return 1
	}
	return 0
}
