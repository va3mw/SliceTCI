package vita

import "testing"

func TestBuildParseRoundTrip(t *testing.T) {
	in := make([]float32, TXFrames*2)
	for i := range in {
		in[i] = float32(i%7)/10 - 0.3
	}
	b := BuildAudio(0x84000000, 5, ClassFloat24k, in)
	if len(b) != (7+TXFrames*2)*4 {
		t.Fatalf("size %d", len(b))
	}
	p, err := Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	if p.StreamID != 0x84000000 || p.OUI != FlexOUI || p.InfoClass != FlexInfoClass || p.PacketClass != ClassFloat24k || p.Count != 5 {
		t.Fatalf("%+v", p)
	}
	out := Stereo(p)
	if len(out) != len(in) {
		t.Fatalf("len %d", len(out))
	}
	for i := range in {
		if out[i] != in[i] {
			t.Fatalf("sample %d %v != %v", i, out[i], in[i])
		}
	}
	if AudioRate(p.PacketClass) != 24000 || FloatClassForRate(48000) != ClassFloat48k {
		t.Error("rate mapping")
	}
}

func TestParseReducedAndTrailer(t *testing.T) {
	// int16 mono, with trailer bit set and a trailer word.
	b := []byte{
		0x1C, 0xD0, 0x00, 0x09, // type 1, C, T, TSI 3, TSF 1, 9 words
		0x04, 0x00, 0x00, 0x08, // stream id
		0x00, 0x00, 0x1C, 0x2D,
		0x53, 0x4C, 0x01, 0x23, // class 0x0123
		0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, // timestamps
		0x40, 0x00, 0xC0, 0x00, // samples 0.5, -0.5
		0, 0, 0, 0, // trailer
	}
	p, err := Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	s := Stereo(p)
	if len(s) != 4 || s[0] < 0.49 || s[0] != s[1] || s[2] > -0.49 {
		t.Fatalf("%v", s)
	}
}
