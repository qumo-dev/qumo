package abr

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// FLV tag types (FLV specification v10.1, E.4.1).
const (
	flvTagVideo  = 9
	flvTagScript = 18
)

// AVC video tag fields (E.4.3.1).
const (
	flvCodecAVC       = 7
	flvFrameKey       = 1
	flvAVCSeqHeader   = 0
	flvAVCNALU        = 1
	flvVideoHeaderLen = 5 // FrameType|CodecID, AVCPacketType, CompositionTime(3)
)

// flvTag is one tag of an FLV stream.
type flvTag struct {
	// Type is the tag type: audio, video or script data.
	Type byte
	// Timestamp is the tag's timestamp in milliseconds.
	Timestamp uint32
	// Data is the tag body.
	Data []byte
}

// flvReader reads the tags of an FLV stream, as ffmpeg's flv muxer writes it
// to a pipe.
type flvReader struct {
	r      *bufio.Reader
	header bool
	buf    [11]byte
}

func newFLVReader(r io.Reader) *flvReader {
	return &flvReader{r: bufio.NewReader(r)}
}

// Next returns the next tag. It returns io.EOF at a clean end of stream,
// between tags.
func (fr *flvReader) Next() (flvTag, error) {
	if !fr.header {
		// FLV header (9 bytes) and PreviousTagSize0 (4 bytes).
		if _, err := io.ReadFull(fr.r, fr.buf[:9]); err != nil {
			return flvTag{}, err
		}
		if string(fr.buf[:3]) != "FLV" {
			return flvTag{}, errors.New("abr: flv: bad signature")
		}
		offset := binary.BigEndian.Uint32(fr.buf[5:9])
		if offset < 9 {
			return flvTag{}, fmt.Errorf("abr: flv: header size %d", offset)
		}
		if _, err := fr.r.Discard(int(offset-9) + 4); err != nil {
			return flvTag{}, noEOF(err)
		}
		fr.header = true
	}

	// io.ReadFull returns io.EOF only when no byte of the tag header was read,
	// that is, at a clean end between tags.
	if _, err := io.ReadFull(fr.r, fr.buf[:11]); err != nil {
		return flvTag{}, err
	}
	tag := flvTag{
		Type: fr.buf[0] & 0x1f,
		// Timestamp (24 bits) then TimestampExtended, the upper 8 bits.
		Timestamp: uint32(fr.buf[4])<<16 | uint32(fr.buf[5])<<8 | uint32(fr.buf[6]) | uint32(fr.buf[7])<<24,
	}
	size := int(fr.buf[1])<<16 | int(fr.buf[2])<<8 | int(fr.buf[3])
	tag.Data = make([]byte, size)
	if _, err := io.ReadFull(fr.r, tag.Data); err != nil {
		return flvTag{}, noEOF(err)
	}
	// PreviousTagSize.
	if _, err := fr.r.Discard(4); err != nil {
		return flvTag{}, noEOF(err)
	}
	return tag, nil
}

// noEOF turns an io.EOF in the middle of a tag into io.ErrUnexpectedEOF.
func noEOF(err error) error {
	if errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}
	return err
}

// isAVC reports whether t is an AVC video tag.
func (t flvTag) isAVC() bool {
	return t.Type == flvTagVideo && len(t.Data) >= flvVideoHeaderLen && t.Data[0]&0x0f == flvCodecAVC
}

// isSequenceHeader reports whether t is an AVC sequence header: the tag that
// carries the AVCDecoderConfigurationRecord.
func (t flvTag) isSequenceHeader() bool {
	return t.isAVC() && t.Data[1] == flvAVCSeqHeader
}

// isFrame reports whether t carries a coded AVC frame.
func (t flvTag) isFrame() bool {
	return t.isAVC() && t.Data[1] == flvAVCNALU
}

// isKeyframe reports whether t is a keyframe tag.
func (t flvTag) isKeyframe() bool {
	return t.isAVC() && t.Data[0]>>4 == flvFrameKey
}

// payload returns the tag body past the AVC video tag header: the
// AVCDecoderConfigurationRecord of a sequence header, or the length-prefixed
// NAL units of a frame.
func (t flvTag) payload() []byte {
	return t.Data[flvVideoHeaderLen:]
}
