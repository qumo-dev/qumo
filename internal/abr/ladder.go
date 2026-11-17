package abr

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// DefaultLadder is the rendition ladder used when ABR_LADDER is unset: 720p at
// 2.5 Mbit/s and 360p at 800 kbit/s, below a source that is typically 1080p.
const DefaultLadder = "720:2500,360:800"

// A Rung is one rendition of the ladder: a picture height and the bitrate the
// encoder targets for it.
type Rung struct {
	// Height is the picture height in pixels. The width follows from the
	// source's aspect ratio.
	Height int
	// Bitrate is the target bitrate in bits per second.
	Bitrate int64
}

// TrackName is the name of the rung's track in the output broadcast, such as
// "video-720p".
func (r Rung) TrackName() string {
	return "video-" + strconv.Itoa(r.Height) + "p"
}

// Size returns the rung's picture size for a source of srcWidth×srcHeight,
// keeping the source's aspect ratio. Both dimensions are even, as 4:2:0
// chroma subsampling requires.
func (r Rung) Size(srcWidth, srcHeight int) (width, height int) {
	height = r.Height &^ 1
	// The nearest even width: twice the nearest whole half-width.
	width = 2 * ((srcWidth*height + srcHeight) / (2 * srcHeight))
	return width, height
}

// ParseLadder parses a ladder description: comma-separated rungs, each a
// height and a bitrate in kbit/s joined by a colon, such as "720:2500,360:800".
// The result is ordered from the tallest rung down.
func ParseLadder(s string) ([]Rung, error) {
	var ladder []Rung
	for part := range strings.SplitSeq(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		h, kbps, ok := strings.Cut(part, ":")
		if !ok {
			return nil, fmt.Errorf("abr: ladder rung %q: want height:kbps", part)
		}
		height, err := strconv.Atoi(h)
		if err != nil || height < 2 {
			return nil, fmt.Errorf("abr: ladder rung %q: bad height", part)
		}
		rate, err := strconv.ParseInt(kbps, 10, 64)
		if err != nil || rate <= 0 {
			return nil, fmt.Errorf("abr: ladder rung %q: bad bitrate", part)
		}
		if slices.ContainsFunc(ladder, func(r Rung) bool { return r.Height == height }) {
			return nil, fmt.Errorf("abr: ladder rung %q: height %d appears twice", part, height)
		}
		ladder = append(ladder, Rung{Height: height, Bitrate: rate * 1000})
	}
	if len(ladder) == 0 {
		return nil, errors.New("abr: empty ladder")
	}
	slices.SortFunc(ladder, func(a, b Rung) int { return cmp.Compare(b.Height, a.Height) })
	return ladder, nil
}

// below returns the rungs of ladder shorter than a source srcHeight tall. A
// rung as tall as the source or taller would only spend bits re-encoding what
// the source track already carries.
func below(ladder []Rung, srcHeight int) []Rung {
	var out []Rung
	for _, r := range ladder {
		if r.Height < srcHeight {
			out = append(out, r)
		}
	}
	return out
}
