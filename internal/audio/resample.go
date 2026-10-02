// Package audio has the sample-rate converter and FIFO used between the
// radio's DAX streams and TCI clients. Internally all audio is interleaved
// stereo float32 at Rate.
package audio

import (
	"math"
	"sync"
)

// Rate is the internal sample rate.
const Rate = 48000

// Resampler converts interleaved float32 audio between sample rates. It uses a
// windowed-sinc low-pass before decimation and linear interpolation. That is
// plenty for the 8/12/24/48 kHz conversions TCI needs on voice-bandwidth audio.
type Resampler struct {
	ch    int
	step  float64 // input frames per output frame
	pos   float64
	hist  []float32 // last input frame
	lp    *fir
	same  bool
	ready bool
}

func NewResampler(inRate, outRate, ch int) *Resampler {
	r := &Resampler{ch: ch, step: float64(inRate) / float64(outRate), hist: make([]float32, ch)}
	r.same = inRate == outRate
	if outRate < inRate {
		r.lp = newFIR(95, 0.45*float64(outRate)/float64(inRate), ch)
	}
	return r
}

func (r *Resampler) Process(in []float32) []float32 {
	if r.same {
		return append([]float32(nil), in...)
	}
	if r.lp != nil {
		in = r.lp.process(in)
	}
	ch := r.ch
	n := len(in) / ch
	if n == 0 {
		return nil
	}
	if !r.ready {
		copy(r.hist, in[:ch])
		r.ready = true
	}
	// Frame k of the extended signal: k==0 is hist, k>=1 is in[k-1].
	at := func(k, c int) float32 {
		if k == 0 {
			return r.hist[c]
		}
		return in[(k-1)*ch+c]
	}
	out := make([]float32, 0, int(float64(n)/r.step+2)*ch)
	for r.pos+1 <= float64(n) {
		i := int(r.pos)
		f := float32(r.pos - float64(i))
		for c := 0; c < ch; c++ {
			a, b := at(i, c), at(i+1, c)
			out = append(out, a+(b-a)*f)
		}
		r.pos += r.step
	}
	r.pos -= float64(n)
	copy(r.hist, in[(n-1)*ch:n*ch])
	return out
}

type fir struct {
	taps  []float32
	ch    int
	delay [][]float32 // per channel circular buffer
	idx   int
}

func newFIR(n int, fc float64, ch int) *fir {
	taps := make([]float32, n)
	m := float64(n - 1)
	var sum float64
	for i := range taps {
		x := float64(i) - m/2
		var s float64
		if x == 0 {
			s = 2 * fc
		} else {
			s = math.Sin(2*math.Pi*fc*x) / (math.Pi * x)
		}
		w := 0.42 - 0.5*math.Cos(2*math.Pi*float64(i)/m) + 0.08*math.Cos(4*math.Pi*float64(i)/m) // Blackman
		taps[i] = float32(s * w)
		sum += s * w
	}
	for i := range taps {
		taps[i] /= float32(sum)
	}
	f := &fir{taps: taps, ch: ch, delay: make([][]float32, ch)}
	for c := range f.delay {
		f.delay[c] = make([]float32, n)
	}
	return f
}

func (f *fir) process(in []float32) []float32 {
	out := make([]float32, len(in))
	n := len(f.taps)
	frames := len(in) / f.ch
	for i := 0; i < frames; i++ {
		f.idx = (f.idx + 1) % n
		for c := 0; c < f.ch; c++ {
			d := f.delay[c]
			d[f.idx] = in[i*f.ch+c]
			var acc float32
			j := f.idx
			for _, t := range f.taps {
				acc += t * d[j]
				j--
				if j < 0 {
					j = n - 1
				}
			}
			out[i*f.ch+c] = acc
		}
	}
	return out
}

// Fifo is a thread-safe FIFO of interleaved stereo float32 frames.
type Fifo struct {
	mu  sync.Mutex
	buf []float32
	max int // max samples; the oldest audio is dropped beyond this
}

func NewFifo(maxFrames int) *Fifo { return &Fifo{max: maxFrames * 2} }

func (f *Fifo) Write(s []float32) {
	f.mu.Lock()
	f.buf = append(f.buf, s...)
	if over := len(f.buf) - f.max; over > 0 {
		over += over & 1
		f.buf = f.buf[over:]
	}
	f.mu.Unlock()
}

// Read fills out, zero-padding on underrun. Returns frames of real audio.
func (f *Fifo) Read(out []float32) int {
	f.mu.Lock()
	n := copy(out, f.buf)
	f.buf = f.buf[n:]
	if len(f.buf) == 0 {
		f.buf = f.buf[:0:0]
	}
	f.mu.Unlock()
	for i := n; i < len(out); i++ {
		out[i] = 0
	}
	return n / 2
}

func (f *Fifo) Frames() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.buf) / 2
}

func (f *Fifo) Reset() {
	f.mu.Lock()
	f.buf = nil
	f.mu.Unlock()
}
