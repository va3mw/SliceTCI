package bridge

import (
	"math"
	"path/filepath"
	"testing"

	"github.com/va3mw/FlexTCI/internal/audio"
	"github.com/va3mw/FlexTCI/internal/config"
)

// TX gain is applied to the audio sent to the radio, and takes effect on
// the next block (no re-key needed).
func TestTXGainApplied(t *testing.T) {
	cfg := config.LoadFrom(filepath.Join(t.TempDir(), "config.json"))
	b := &Bridge{cfg: cfg, fifo: audio.NewFifo(1000)}
	block := func() float32 {
		b.fifo.Write([]float32{0.5, 0.5})
		out := make([]float32, 2)
		b.pullTX(out)
		return out[0]
	}
	// A fresh config defaults to -6 dB.
	if v := block(); math.Abs(float64(v)-0.2506) > 0.001 {
		t.Fatalf("default -6 dB: got %v, want ~0.25", v)
	}
	cfg.Update(func(c *config.Config) { c.TXGainDB = 0 })
	if v := block(); v != 0.5 {
		t.Fatalf("0 dB: got %v", v)
	}
	if peak := math.Float32frombits(b.txPeak.Load()); peak < 0.49 {
		t.Errorf("TX meter peak %v not recorded", peak)
	}
}
