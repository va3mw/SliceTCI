// Package vita encodes and decodes the VITA-49 packets FlexRadio uses for
// DAX audio streams.
package vita

import (
	"encoding/binary"
	"errors"
	"math"
)

const (
	FlexOUI       = 0x001C2D
	FlexInfoClass = 0x534C

	TypeIFDataWithStream = 1

	// DAX audio packet classes. Full bandwidth is big-endian float32 stereo;
	// reduced bandwidth is big-endian int16 mono.
	ClassFloat24k  = 0x03E3
	ClassFloat48k  = 0x03E4
	ClassFloat96k  = 0x03E5
	ClassInt16_24k = 0x0123
	ClassInt16_48k = 0x0124
	ClassInt16_96k = 0x0125

	// TXFrames is the number of stereo frames per DAX TX packet.
	TXFrames = 128
)

type Packet struct {
	Type        uint8
	Count       uint8
	StreamID    uint32
	OUI         uint32
	InfoClass   uint16
	PacketClass uint16
	Payload     []byte
}

// Parse decodes the VITA-49 preamble and returns the payload slice.
func Parse(b []byte) (Packet, error) {
	var p Packet
	if len(b) < 4 {
		return p, errors.New("short packet")
	}
	be := binary.BigEndian
	h := be.Uint32(b)
	p.Type = uint8(h >> 28)
	p.Count = uint8(h>>16) & 0x0F
	size := int(h&0xFFFF) * 4
	if size > len(b) || size < 4 {
		size = len(b)
	}
	i := 4
	need := func(n int) bool { return i+n <= size }
	if p.Type == 1 || p.Type == 3 {
		if !need(4) {
			return p, errors.New("truncated stream id")
		}
		p.StreamID = be.Uint32(b[i:])
		i += 4
	}
	if h&(1<<27) != 0 {
		if !need(8) {
			return p, errors.New("truncated class id")
		}
		p.OUI = be.Uint32(b[i:]) & 0xFFFFFF
		c := be.Uint32(b[i+4:])
		p.InfoClass, p.PacketClass = uint16(c>>16), uint16(c)
		i += 8
	}
	if (h>>22)&3 != 0 {
		i += 4
	}
	if (h>>20)&3 != 0 {
		i += 8
	}
	end := size
	if h&(1<<26) != 0 { // trailer
		end -= 4
	}
	if i > end {
		return p, errors.New("truncated packet")
	}
	p.Payload = b[i:end]
	return p, nil
}

// AudioRate returns the sample rate of a DAX audio class, or 0 if the
// packet is not DAX audio.
func AudioRate(class uint16) int {
	switch class {
	case ClassFloat24k, ClassInt16_24k:
		return 24000
	case ClassFloat48k, ClassInt16_48k:
		return 48000
	case ClassFloat96k, ClassInt16_96k:
		return 96000
	}
	return 0
}

// Stereo converts a DAX audio payload to interleaved stereo float32.
func Stereo(p Packet) []float32 {
	be := binary.BigEndian
	switch p.PacketClass {
	case ClassFloat24k, ClassFloat48k, ClassFloat96k:
		n := len(p.Payload) / 4
		out := make([]float32, n&^1)
		for i := range out {
			out[i] = math.Float32frombits(be.Uint32(p.Payload[i*4:]))
		}
		return out
	case ClassInt16_24k, ClassInt16_48k, ClassInt16_96k:
		n := len(p.Payload) / 2
		out := make([]float32, n*2)
		for i := 0; i < n; i++ {
			v := float32(int16(be.Uint16(p.Payload[i*2:]))) / 32767
			out[2*i], out[2*i+1] = v, v
		}
		return out
	}
	return nil
}

// FloatClassForRate returns the full-bandwidth class for a slice rate, or 0.
func FloatClassForRate(rate int) uint16 {
	switch rate {
	case 24000:
		return ClassFloat24k
	case 48000:
		return ClassFloat48k
	case 96000:
		return ClassFloat96k
	}
	return 0
}

// BuildAudio encodes one DAX TX packet of interleaved stereo float32.
func BuildAudio(streamID uint32, count uint8, class uint16, stereo []float32) []byte {
	words := 7 + len(stereo)
	b := make([]byte, words*4)
	be := binary.BigEndian
	h := uint32(TypeIFDataWithStream)<<28 | 1<<27 | // class id present, no trailer
		3<<22 | 1<<20 | // TSI other, TSF sample count
		uint32(count&0x0F)<<16 | uint32(words)
	be.PutUint32(b[0:], h)
	be.PutUint32(b[4:], streamID)
	be.PutUint32(b[8:], FlexOUI)
	be.PutUint32(b[12:], uint32(FlexInfoClass)<<16|uint32(class))
	// timestamps (bytes 16..27) left zero
	for i, v := range stereo {
		be.PutUint32(b[28+i*4:], math.Float32bits(v))
	}
	return b
}
