package audio

import (
	"math"
	"testing"
)

func tone(rate, frames int, hz float64) []float32 {
	s := make([]float32, frames*2)
	for i := 0; i < frames; i++ {
		v := float32(0.5 * math.Sin(2*math.Pi*hz*float64(i)/float64(rate)))
		s[2*i], s[2*i+1] = v, v
	}
	return s
}

func rms(s []float32) float64 {
	var a float64
	for _, v := range s {
		a += float64(v) * float64(v)
	}
	return math.Sqrt(a / float64(len(s)))
}

func TestResampleRatesAndLevel(t *testing.T) {
	for _, out := range []int{8000, 12000, 24000} {
		r := NewResampler(48000, out, 2)
		var got []float32
		in := tone(48000, 48000, 1000)
		for i := 0; i < len(in); i += 960 { // feed in 10 ms chunks
			got = append(got, r.Process(in[i:i+960])...)
		}
		frames := len(got) / 2
		if frames < out-2 || frames > out+2 {
			t.Errorf("48k->%d: %d frames for 1 s", out, frames)
		}
		if l := rms(got[len(got)/2:]); math.Abs(l-0.3536) > 0.02 {
			t.Errorf("48k->%d: 1 kHz tone level %.3f", out, l)
		}
	}
	// Upsampling
	r := NewResampler(12000, 48000, 2)
	got := r.Process(tone(12000, 12000, 1000))
	if f := len(got) / 2; f < 47990 || f > 48000 {
		t.Errorf("12k->48k: %d frames", f)
	}
}

func TestResampleRejectsAlias(t *testing.T) {
	// 10 kHz cannot be represented at 12 kHz; it must be filtered out.
	r := NewResampler(48000, 12000, 2)
	got := r.Process(tone(48000, 48000, 10000))
	if l := rms(got[len(got)/2:]); l > 0.02 {
		t.Errorf("alias leaked through: rms %.3f", l)
	}
}

func TestFifo(t *testing.T) {
	f := NewFifo(4)
	f.Write([]float32{1, 1, 2, 2, 3, 3})
	out := make([]float32, 8)
	if n := f.Read(out); n != 3 || out[4] != 3 || out[6] != 0 {
		t.Errorf("read %d %v", n, out)
	}
	f.Write(make([]float32, 20)) // overflow keeps newest 4 frames
	if f.Frames() != 4 {
		t.Errorf("frames %d", f.Frames())
	}
}
