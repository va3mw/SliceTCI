package bridge

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/va3mw/FlexTCI/internal/audio"
	"github.com/va3mw/FlexTCI/internal/flex"
	"github.com/va3mw/FlexTCI/internal/logx"
	"github.com/va3mw/FlexTCI/internal/vita"
)

// directRX receives a slice's audio straight from the radio as a dax_rx
// VITA-49 stream, so SmartSDR DAX and the Windows sound devices are not
// needed. The stream is recreated after a reconnect or if the radio drops it.
type directRX struct {
	stop chan struct{}
	wg   sync.WaitGroup
}

func startDirectRX(r *flex.Radio, lg *logx.Source, dax int, onData func([]float32)) *directRX {
	d := &directRX{stop: make(chan struct{})}
	var packets atomic.Uint64
	// The handler runs on the radio's single UDP goroutine, so the
	// resamplers need no locking.
	resamplers := map[int]*audio.Resampler{}
	handler := func(p vita.Packet) {
		rate := vita.AudioRate(p.PacketClass)
		if rate == 0 {
			return
		}
		s := vita.Stereo(p)
		if rate != audio.Rate {
			rs := resamplers[rate]
			if rs == nil {
				rs = audio.NewResampler(rate, audio.Rate, 2)
				resamplers[rate] = rs
			}
			s = rs.Process(s)
		}
		if packets.Add(1) == 1 {
			lg.Info("receiving DAX channel %d audio from the radio (%d Hz)", dax, rate)
		}
		onData(s)
	}
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		var id uint32
		var epoch uint64
		var created time.Time
		var lastErr string
		warnedSilent := false
		t := time.NewTicker(500 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-d.stop:
				if id != 0 && r.StreamAlive(id) {
					r.RemoveStream(id)
				}
				return
			case <-t.C:
			}
			if id != 0 && (r.Epoch() != epoch || !r.StreamAlive(id)) {
				lg.Info("dax_rx stream for DAX %d lost; will recreate", dax)
				id = 0
			}
			if id == 0 {
				if !r.Connected() {
					continue
				}
				nid, err := r.CreateStream(fmt.Sprintf("type=dax_rx dax_channel=%d", dax), handler)
				if err != nil {
					if err.Error() != lastErr {
						lg.Error("cannot open DAX channel %d on the radio: %v", dax, err)
						lastErr = err.Error()
					}
					continue
				}
				id, epoch, created, lastErr = nid, r.Epoch(), time.Now(), ""
				packets.Store(0)
				warnedSilent = false
				continue
			}
			if !warnedSilent && packets.Load() == 0 && time.Since(created) > 3*time.Second {
				lg.Warn("no audio packets on DAX channel %d yet: check the slice's DAX channel is %d and that Windows Firewall allows FlexTCI (UDP)", dax, dax)
				warnedSilent = true
			}
		}
	}()
	return d
}

func (d *directRX) Stop() {
	close(d.stop)
	d.wg.Wait()
}

// directTX owns one dax_tx stream per radio session and paces TX audio to
// the radio in 128-frame VITA-49 packets at the TX slice's sample rate.
type directTX struct {
	r  *flex.Radio
	lg *logx.Source

	mu    sync.Mutex
	id    uint32
	epoch uint64
}

func newDirectTX(r *flex.Radio, lg *logx.Source) *directTX {
	return &directTX{r: r, lg: lg}
}

// ensure returns a live dax_tx stream id, creating one if needed.
func (d *directTX) ensure() (uint32, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.id != 0 && d.epoch == d.r.Epoch() && d.r.StreamAlive(d.id) {
		return d.id, nil
	}
	id, err := d.r.CreateStream("type=dax_tx", nil)
	if err != nil {
		return 0, err
	}
	d.id, d.epoch = id, d.r.Epoch()
	return id, nil
}

// txSliceRate returns the DAX rate of the bound station's TX slice.
func txSliceRate(r *flex.Radio) int {
	s := r.Snapshot()
	for _, sl := range s.Slices {
		if sl.TX && sl.ClientHandle == s.BoundHandle && sl.SampleRate > 0 {
			return sl.SampleRate
		}
	}
	return 24000
}

type directTXSession struct {
	stop chan struct{}
	wg   sync.WaitGroup
}

func (s *directTXSession) Stop() {
	close(s.stop)
	s.wg.Wait()
}

// start begins sending audio pulled from pull (stereo, 48 kHz) until Stop.
func (d *directTX) start(pull func([]float32)) stopper {
	s := &directTXSession{stop: make(chan struct{})}
	id, err := d.ensure()
	if err != nil {
		d.lg.Error("cannot open DAX TX stream on the radio: %v", err)
	} else if !d.r.StreamOwnsTX(id) {
		d.r.Command(fmt.Sprintf("stream set 0x%08X tx=1", id), nil)
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		t := time.NewTicker(5 * time.Millisecond)
		defer t.Stop()
		var (
			rate    int
			rs      *audio.Resampler
			pending []float32
			start   time.Time
			sent    int // frames sent at the slice rate since start
			count   uint8
			packets int
			warned  bool
		)
		began := time.Now()
		for {
			select {
			case <-s.stop:
				d.lg.Debug("DAX TX: sent %d packets", packets)
				return
			case <-t.C:
			}
			if id == 0 {
				if id, err = d.ensure(); err != nil {
					continue
				}
			}
			if r := txSliceRate(d.r); r != rate || rs == nil {
				rate, rs, pending = r, audio.NewResampler(audio.Rate, r, 2), nil
				start, sent = time.Now(), 0
			}
			class := vita.FloatClassForRate(rate)
			if class == 0 {
				continue
			}
			due := int(time.Since(start).Seconds()*float64(rate)) - sent
			if due > rate/10 { // fell behind (e.g. a stall): resync instead of bursting
				sent += due - vita.TXFrames
				due = vita.TXFrames
			}
			for due >= vita.TXFrames {
				for len(pending) < vita.TXFrames*2 {
					chunk := make([]float32, (vita.TXFrames*audio.Rate/rate+1)*2)
					pull(chunk)
					pending = append(pending, rs.Process(chunk)...)
				}
				pkt := pending[:vita.TXFrames*2]
				pending = append([]float32(nil), pending[vita.TXFrames*2:]...)
				sent += vita.TXFrames
				due -= vita.TXFrames
				if !d.r.StreamOwnsTX(id) {
					if !warned && time.Since(began) > time.Second {
						d.lg.Warn("radio has not given FlexTCI DAX TX ownership (another DAX client may own it)")
						warned = true
					}
					continue
				}
				if err := d.r.SendVITA(vita.BuildAudio(id, count, class, pkt)); err != nil {
					d.lg.Error("DAX TX send failed: %v", err)
				}
				count++
				packets++
			}
		}
	}()
	return s
}
