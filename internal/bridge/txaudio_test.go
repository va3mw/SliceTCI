package bridge

import (
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/va3mw/SliceTCI/internal/config"
	"github.com/va3mw/SliceTCI/internal/logx"
	"github.com/va3mw/SliceTCI/internal/tci"
	"github.com/va3mw/SliceTCI/internal/vita"
)

// runTone keys TCI-audio TX and answers every TX_CHRONO after delay with a
// steady 1 kHz tone for 3 s. It returns the fraction of frames the radio got
// that were silent, and the unkey log line.
func runTone(t *testing.T, port int, delay time.Duration) (silentFrac float64, unkey string) {
	radio, b := newTestBridge(t, port, func(c *config.Config) { c.TXGainDB = 0 })
	var lineMu sync.Mutex
	b.log.OnEntry(func(e logx.Entry) {
		if strings.HasPrefix(e.Msg, "unkeyed") {
			lineMu.Lock()
			unkey = e.Msg
			lineMu.Unlock()
		}
	})
	startTestBridge(t, radio, b)
	c := dialTCI(t, "127.0.0.1", port)
	c.expect(t, "ready;", 3*time.Second)
	c.send("TRX:0,true,tci;")
	radio.expect(t, "xmit 1", time.Second)

	var mu sync.Mutex
	phase := 0.0
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			case h := <-c.chrono:
				go func(h tci.Header) {
					time.Sleep(delay)
					mu.Lock()
					defer mu.Unlock()
					buf := make([]float32, h.Length)
					for k := 0; k+1 < len(buf); k += 2 {
						v := float32(0.5 * math.Sin(phase))
						buf[k], buf[k+1] = v, v
						phase += 2 * math.Pi * 1000 / 48000
					}
					h.Type = tci.StreamTXAudio
					c.conn.WriteMessage(websocket.BinaryMessage, tci.Encode(h, buf))
				}(h)
			}
		}
	}()
	time.Sleep(500 * time.Millisecond) // past the start of the transmission
	for len(radio.txPackets) > 0 {
		<-radio.txPackets
	}
	end := time.After(3 * time.Second)
	frames, silent := 0, 0
loop:
	for {
		select {
		case p := <-radio.txPackets:
			s := vita.Stereo(p)
			for k := 0; k+1 < len(s); k += 2 {
				frames++
				if s[k] == 0 && s[k+1] == 0 {
					silent++
				}
			}
		case <-end:
			break loop
		}
	}
	close(stop)
	mu.Lock() // answers still in flight write on the same connection
	c.send("TRX:0,false;")
	mu.Unlock()
	radio.expect(t, "xmit 0", time.Second)
	time.Sleep(50 * time.Millisecond)
	lineMu.Lock()
	defer lineMu.Unlock()
	return float64(silent) / float64(frames), unkey
}

// TestTXAudioSteady: a client answering TX_CHRONO promptly or slowly (up to
// well past the 50 ms buffering target) gives the radio unbroken audio, and
// the unkey line says so.
func TestTXAudioSteady(t *testing.T) {
	for i, delay := range []time.Duration{0, 30 * time.Millisecond, 70 * time.Millisecond, 150 * time.Millisecond} {
		t.Run(delay.String(), func(t *testing.T) {
			frac, line := runTone(t, 50960+i, delay)
			t.Logf("%s", line)
			if frac != 0 {
				t.Errorf("%.2f%% of TX frames were silent", 100*frac)
			}
			if !strings.Contains(line, "no underruns") || !strings.Contains(line, "client answers avg") {
				t.Errorf("unkey line lacks TX audio health or has underruns: %q", line)
			}
		})
	}
}

// TestTXAudioUnderrunReported: a client slower than the pacing covers
// (maxReplyAllowance) causes gaps, and the unkey line reports them.
func TestTXAudioUnderrunReported(t *testing.T) {
	frac, line := runTone(t, 50965, 400*time.Millisecond)
	t.Logf("%s", line)
	if frac == 0 {
		t.Fatal("expected gaps from a 400 ms client")
	}
	if !strings.Contains(line, " underruns (") {
		t.Errorf("gaps (%.2f%% silent) not reported in the unkey line: %q", 100*frac, line)
	}
}
