package abr

import (
	"bytes"
	"encoding/binary"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// flvStream builds an FLV stream of tags.
func flvStream(tags ...flvTag) []byte {
	var b bytes.Buffer
	b.Write([]byte{'F', 'L', 'V', 1, 1, 0, 0, 0, 9})
	b.Write([]byte{0, 0, 0, 0}) // PreviousTagSize0
	for _, t := range tags {
		n := len(t.Data)
		b.Write([]byte{t.Type, byte(n >> 16), byte(n >> 8), byte(n),
			byte(t.Timestamp >> 16), byte(t.Timestamp >> 8), byte(t.Timestamp), byte(t.Timestamp >> 24),
			0, 0, 0})
		b.Write(t.Data)
		_ = binary.Write(&b, binary.BigEndian, uint32(11+n))
	}
	return b.Bytes()
}

func TestFLVReader(t *testing.T) {
	seq := flvTag{Type: flvTagVideo, Timestamp: 0, Data: []byte{0x17, 0, 0, 0, 0, 0x01, 0x64}}
	key := flvTag{Type: flvTagVideo, Timestamp: 5_000, Data: []byte{0x17, 1, 0, 0, 0, 0, 0, 0, 1, 0x65}}
	inter := flvTag{Type: flvTagVideo, Timestamp: 0x01_02_03_04, Data: []byte{0x27, 1, 0, 0, 0, 0, 0, 0, 1, 0x41}}
	script := flvTag{Type: flvTagScript, Data: []byte{2, 0, 0}}

	fr := newFLVReader(bytes.NewReader(flvStream(script, seq, key, inter)))
	var got []flvTag
	for {
		tag, err := fr.Next()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		got = append(got, tag)
	}
	require.Len(t, got, 4)
	assert.Equal(t, []flvTag{script, seq, key, inter}, got)

	assert.False(t, got[0].isAVC())
	assert.True(t, got[1].isSequenceHeader())
	assert.False(t, got[1].isFrame())
	assert.Equal(t, []byte{0x01, 0x64}, got[1].payload())
	assert.True(t, got[2].isFrame())
	assert.True(t, got[2].isKeyframe())
	assert.True(t, got[3].isFrame())
	assert.False(t, got[3].isKeyframe())
	assert.Equal(t, uint32(0x01_02_03_04), got[3].Timestamp, "the extended byte is the timestamp's top 8 bits")
}

func TestFLVReaderTruncated(t *testing.T) {
	stream := flvStream(flvTag{Type: flvTagVideo, Data: []byte{0x17, 1, 0, 0, 0, 1, 2, 3}})
	for _, cut := range []int{len(stream) - 1, len(stream) - 6, 13 + 5} {
		_, err := newFLVReader(bytes.NewReader(stream[:cut])).Next()
		assert.ErrorIs(t, err, io.ErrUnexpectedEOF, "cut at %d", cut)
	}
}

func TestFLVReaderBadSignature(t *testing.T) {
	_, err := newFLVReader(bytes.NewReader([]byte("NOTFLV\x00\x00\x00\x09\x00\x00\x00\x00"))).Next()
	assert.Error(t, err)
}
