// Package tci holds TCI protocol primitives: text command parsing, the 64-byte
// binary stream header, sample encoding and mode-name mapping.
package tci

import (
	"encoding/binary"
	"errors"
	"math"
	"strconv"
	"strings"
)

type Command struct {
	Name string // lower case
	Args []string
	Raw  string
}

// Parse splits a text frame into commands ("name:a,b;name2;").
func Parse(frame string) []Command {
	var out []Command
	for _, part := range strings.Split(frame, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, args, _ := strings.Cut(part, ":")
		c := Command{Name: strings.ToLower(strings.TrimSpace(name)), Raw: part}
		if args != "" {
			for _, a := range strings.Split(args, ",") {
				c.Args = append(c.Args, strings.TrimSpace(a))
			}
		}
		out = append(out, c)
	}
	return out
}

// Fmt builds "name:a,b;" from arguments.
func Fmt(name string, args ...any) string {
	var b strings.Builder
	b.WriteString(name)
	for i, a := range args {
		if i == 0 {
			b.WriteByte(':')
		} else {
			b.WriteByte(',')
		}
		switch v := a.(type) {
		case string:
			b.WriteString(v)
		case int:
			b.WriteString(strconv.Itoa(v))
		case int64:
			b.WriteString(strconv.FormatInt(v, 10))
		case bool:
			b.WriteString(strconv.FormatBool(v))
		}
	}
	b.WriteByte(';')
	return b.String()
}

func ParseBool(s string) (bool, bool) {
	switch strings.ToLower(s) {
	case "true", "1":
		return true, true
	case "false", "0":
		return false, true
	}
	return false, false
}

// ---- binary streams ----

const HeaderSize = 64

const (
	StreamIQ       = 0
	StreamRXAudio  = 1
	StreamTXAudio  = 2
	StreamTXChrono = 3
	StreamLineOut  = 4
)

const (
	FmtInt16   = 0
	FmtInt24   = 1
	FmtInt32   = 2
	FmtFloat32 = 3
)

type Header struct {
	Receiver   uint32
	SampleRate uint32
	Format     uint32
	Codec      uint32
	CRC        uint32
	Length     uint32 // total samples (frames * channels)
	Type       uint32
	Channels   uint32
}

func (h Header) Put(b []byte) {
	le := binary.LittleEndian
	le.PutUint32(b[0:], h.Receiver)
	le.PutUint32(b[4:], h.SampleRate)
	le.PutUint32(b[8:], h.Format)
	le.PutUint32(b[12:], h.Codec)
	le.PutUint32(b[16:], h.CRC)
	le.PutUint32(b[20:], h.Length)
	le.PutUint32(b[24:], h.Type)
	le.PutUint32(b[28:], h.Channels)
	for i := 32; i < HeaderSize; i++ {
		b[i] = 0
	}
}

func ReadHeader(b []byte) (Header, error) {
	if len(b) < HeaderSize {
		return Header{}, errors.New("short binary frame")
	}
	le := binary.LittleEndian
	return Header{
		Receiver:   le.Uint32(b[0:]),
		SampleRate: le.Uint32(b[4:]),
		Format:     le.Uint32(b[8:]),
		Codec:      le.Uint32(b[12:]),
		CRC:        le.Uint32(b[16:]),
		Length:     le.Uint32(b[20:]),
		Type:       le.Uint32(b[24:]),
		Channels:   le.Uint32(b[28:]),
	}, nil
}

func BytesPerSample(format uint32) int {
	switch format {
	case FmtInt16:
		return 2
	case FmtInt24:
		return 3
	default:
		return 4
	}
}

func FormatByName(s string) (uint32, bool) {
	switch strings.ToLower(s) {
	case "int16":
		return FmtInt16, true
	case "int24":
		return FmtInt24, true
	case "int32":
		return FmtInt32, true
	case "float32":
		return FmtFloat32, true
	}
	return 0, false
}

func FormatName(f uint32) string {
	return [...]string{"int16", "int24", "int32", "float32"}[f&3]
}

// Encode builds a complete binary frame (header + samples).
func Encode(h Header, samples []float32) []byte {
	bps := BytesPerSample(h.Format)
	b := make([]byte, HeaderSize+len(samples)*bps)
	h.Length = uint32(len(samples))
	h.Put(b)
	d := b[HeaderSize:]
	le := binary.LittleEndian
	for i, v := range samples {
		if v > 1 {
			v = 1
		} else if v < -1 {
			v = -1
		}
		switch h.Format {
		case FmtInt16:
			le.PutUint16(d[i*2:], uint16(int16(v*32767)))
		case FmtInt24:
			x := int32(v * 8388607)
			d[i*3], d[i*3+1], d[i*3+2] = byte(x), byte(x>>8), byte(x>>16)
		case FmtInt32:
			le.PutUint32(d[i*4:], uint32(int32(float64(v)*2147483647)))
		default:
			le.PutUint32(d[i*4:], math.Float32bits(v))
		}
	}
	return b
}

// Decode returns the samples carried in a frame, limited by the data present.
func Decode(h Header, data []byte) []float32 {
	bps := BytesPerSample(h.Format)
	n := int(h.Length)
	if n*bps > len(data) {
		n = len(data) / bps
	}
	out := make([]float32, n)
	le := binary.LittleEndian
	for i := 0; i < n; i++ {
		switch h.Format {
		case FmtInt16:
			out[i] = float32(int16(le.Uint16(data[i*2:]))) / 32768
		case FmtInt24:
			x := int32(data[i*3]) | int32(data[i*3+1])<<8 | int32(int8(data[i*3+2]))<<16
			out[i] = float32(x) / 8388608
		case FmtInt32:
			out[i] = float32(float64(int32(le.Uint32(data[i*4:]))) / 2147483648)
		default:
			out[i] = math.Float32frombits(le.Uint32(data[i*4:]))
		}
	}
	return out
}

// ---- modes ----

// Modes advertised in MODULATIONS_LIST. Flex-only modes use their Flex names.
var ModeList = []string{"am", "sam", "lsb", "usb", "cw", "nfm", "digl", "digu", "dfm", "rtty", "freedv"}

// ModesFromFlex converts a slice's mode_list into TCI names, keeping the
// radio's order and dropping duplicates (FM and NFM both map to nfm) and
// modes TCIToFlex cannot map back.
func ModesFromFlex(list []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range list {
		t := FlexToTCI(strings.TrimSpace(m))
		if _, ok := TCIToFlex(t); !ok || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	if len(out) == 0 {
		return ModeList
	}
	return out
}

// FlexToTCI maps a Flex slice mode to a TCI modulation name.
func FlexToTCI(m string) string {
	switch strings.ToUpper(m) {
	case "FM", "NFM":
		return "nfm"
	case "FDV":
		return "freedv"
	case "CWL", "CWU":
		return "cw"
	}
	return strings.ToLower(m)
}

// TCIToFlex maps a TCI modulation name to a Flex mode. ok=false if unsupported.
func TCIToFlex(m string) (string, bool) {
	switch strings.ToLower(m) {
	case "am":
		return "AM", true
	case "sam":
		return "SAM", true
	case "lsb":
		return "LSB", true
	case "usb":
		return "USB", true
	case "cw":
		return "CW", true
	case "nfm", "fm":
		return "FM", true
	case "digl":
		return "DIGL", true
	case "digu":
		return "DIGU", true
	case "dfm":
		return "DFM", true
	case "rtty":
		return "RTTY", true
	case "freedv", "fdv":
		return "FDV", true
	}
	return "", false
}
