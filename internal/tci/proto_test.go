package tci

import (
	"math"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	cmds := Parse("VFO:0,0,14074000;trx:0,true,tci; READY;")
	if len(cmds) != 3 {
		t.Fatalf("got %d commands", len(cmds))
	}
	if cmds[0].Name != "vfo" || len(cmds[0].Args) != 3 || cmds[0].Args[2] != "14074000" {
		t.Errorf("bad vfo: %+v", cmds[0])
	}
	if cmds[1].Args[2] != "tci" || cmds[2].Name != "ready" || cmds[2].Args != nil {
		t.Errorf("bad parse: %+v", cmds)
	}
}

func TestFmt(t *testing.T) {
	if s := Fmt("vfo", 0, 1, int64(7074000)); s != "vfo:0,1,7074000;" {
		t.Error(s)
	}
	if s := Fmt("ready"); s != "ready;" {
		t.Error(s)
	}
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	in := []float32{0, 0.5, -0.5, 0.999, -1}
	for _, f := range []uint32{FmtInt16, FmtInt24, FmtInt32, FmtFloat32} {
		h := Header{SampleRate: 48000, Format: f, Type: StreamRXAudio, Channels: 1}
		b := Encode(h, in)
		got, err := ReadHeader(b)
		if err != nil || got.Length != uint32(len(in)) || got.Format != f {
			t.Fatalf("%s header %+v %v", FormatName(f), got, err)
		}
		out := Decode(got, b[HeaderSize:])
		for i := range in {
			if math.Abs(float64(out[i]-in[i])) > 1e-3 {
				t.Errorf("%s sample %d: %v != %v", FormatName(f), i, out[i], in[i])
			}
		}
	}
}

func TestModesFromFlex(t *testing.T) {
	got := strings.Join(ModesFromFlex([]string{"LSB", "USB", "AM", "CW", "DIGL", "DIGU", "SAM", "FM", "NFM", "DFM", "RTTY", "XYZ"}), ",")
	if got != "lsb,usb,am,cw,digl,digu,sam,nfm,dfm,rtty" {
		t.Error(got)
	}
}

func TestModes(t *testing.T) {
	for _, m := range ModeList {
		flex, ok := TCIToFlex(m)
		if !ok {
			t.Errorf("advertised mode %s not mapped", m)
			continue
		}
		if back := FlexToTCI(flex); back != m {
			t.Errorf("%s -> %s -> %s", m, flex, back)
		}
	}
}
