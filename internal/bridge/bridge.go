// Package bridge connects TCI channels to the radio and the DAX audio devices,
// and owns transmit arbitration and the safety unkey.
package bridge

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/va3mw/FlexTCI/internal/audio"
	"github.com/va3mw/FlexTCI/internal/config"
	"github.com/va3mw/FlexTCI/internal/flex"
	"github.com/va3mw/FlexTCI/internal/logx"
	"github.com/va3mw/FlexTCI/internal/tci"
)

type stopper interface{ Stop() }

// txOwner is the single TCI client currently keying the transmitter.
// Lock order: Bridge.mu may be taken before client.mu, never after
// Channel.mu is released into bridge calls (bridge never calls a Channel
// while holding Bridge.mu).
type txOwner struct {
	c           *client
	tciAudio    bool
	tune        bool
	sliceIdx    int
	since       time.Time
	lastAudio   time.Time
	lastChrono  time.Time
	outstanding int // 48 kHz frames requested by TX_CHRONO and not yet received
	restoreDAX  bool
	chronos     int // TX_CHRONO requests sent this session
	blocks      int // TX audio blocks received this session
}

type Bridge struct {
	log   *logx.Logger
	lg    *logx.Source
	cfg   *config.Store
	radio *flex.Radio
	disc  *flex.Discovery
	onUI  func()

	chMu     sync.Mutex
	channels [config.MaxChannels]*Channel

	mu     sync.Mutex
	tx     *txOwner
	render stopper
	fifo   *audio.Fifo

	// newRender starts sending TX audio to the radio; replaced in tests.
	newRender func(pull func([]float32)) stopper
	dtx       *directTX
	txPeak    atomic.Uint32 // float32 bits, TX audio peak since last status
	dtxEpoch  atomic.Uint64 // radio session the dax_tx stream was prepared for

	uiDirty atomic.Bool
	quit    chan struct{}
}

func New(l *logx.Logger, cfg *config.Store, onUI func()) *Bridge {
	b := &Bridge{log: l, lg: l.Src("bridge"), cfg: cfg, onUI: onUI, quit: make(chan struct{})}
	b.fifo = audio.NewFifo(audio.Rate) // up to 1 s of TX audio
	b.radio = flex.NewRadio(l, b.onRadioUpdate)
	b.dtx = newDirectTX(b.radio, l.Src("dax-tx"))
	b.newRender = b.dtx.start
	b.disc = flex.NewDiscovery(l, b.notifyUI)
	return b
}

func (b *Bridge) Start() {
	b.disc.Start()
	b.ApplyChannels()
	go b.txLoop()
	go b.uiLoop()
	c := b.cfg.Get()
	if c.AutoConnect && (c.RadioSerial != "" || c.RadioIP != "") {
		go func() {
			// Give discovery a moment so we can match the radio by serial.
			for i := 0; i < 20 && c.RadioSerial != ""; i++ {
				if _, ok := b.disc.Find(c.RadioSerial); ok {
					break
				}
				time.Sleep(250 * time.Millisecond)
			}
			b.Connect(c.RadioSerial, c.RadioIP)
		}()
	}
}

func (b *Bridge) Shutdown() {
	close(b.quit)
	b.releaseAll("shutting down")
	b.chMu.Lock()
	for i, ch := range b.channels {
		if ch != nil {
			ch.stop()
			b.channels[i] = nil
		}
	}
	b.chMu.Unlock()
	b.radio.Disconnect()
	b.disc.Stop()
}

// Connect to a radio by discovery serial, or by IP when serial is "".
func (b *Bridge) Connect(serial, ip string) error {
	model := ""
	if serial != "" {
		r, ok := b.disc.Find(serial)
		if !ok {
			if ip == "" {
				return fmt.Errorf("radio %s not found on the network", serial)
			}
			b.lg.Warn("radio %s not seen in discovery; trying last IP %s", serial, ip)
		} else {
			ip, model = r.IP, r.Model
		}
	}
	if ip == "" {
		return errors.New("no radio selected")
	}
	b.cfg.Update(func(c *config.Config) { c.RadioSerial, c.RadioIP, c.AutoConnect = serial, ip, true })
	b.radio.Connect(ip, model, b.cfg.Get().Station)
	b.notifyUI()
	return nil
}

func (b *Bridge) Disconnect() {
	b.releaseAll("radio disconnect requested")
	b.cfg.Update(func(c *config.Config) { c.AutoConnect = false })
	b.radio.Disconnect()
	b.notifyUI()
}

func (b *Bridge) SelectStation(name string) {
	b.releaseAll("station changed")
	b.cfg.Update(func(c *config.Config) { c.Station = name })
	b.radio.SetStation(name)
}

// ApplyChannels starts, stops or restarts listeners to match the config.
// Channels whose settings did not change keep their clients.
func (b *Bridge) ApplyChannels() {
	c := b.cfg.Get()
	b.chMu.Lock()
	var started []*Channel
	for i := 0; i < config.MaxChannels; i++ {
		want := i < c.ChannelCount
		cur := b.channels[i]
		if cur != nil && want && cur.cfg.SameExceptGain(c.Channels[i]) {
			cur.setRXGain(c.Channels[i].RXGain()) // gain changes don't drop clients
			continue
		}
		if cur != nil {
			cur.stop()
			b.channels[i] = nil
			cur = nil
		}
		if want && cur == nil {
			ch := newChannel(b, i, c.Channels[i])
			ch.start()
			b.channels[i] = ch
			started = append(started, ch)
		}
	}
	b.chMu.Unlock()
	snap := b.radio.Snapshot()
	for _, ch := range started {
		ch.sync(&snap)
	}
	b.notifyUI()
}

func (b *Bridge) channelList() []*Channel {
	b.chMu.Lock()
	defer b.chMu.Unlock()
	var out []*Channel
	for _, ch := range b.channels {
		if ch != nil {
			out = append(out, ch)
		}
	}
	return out
}

func (b *Bridge) onRadioUpdate() {
	snap := b.radio.Snapshot()
	// Create the dax_tx stream once per session, as soon as we are bound, so
	// the first key does not wait for the radio.
	if snap.Connected && snap.BoundHandle != 0 {
		if e := b.radio.Epoch(); b.dtxEpoch.Swap(e) != e {
			go b.dtx.prepare()
		}
	}
	for _, ch := range b.channelList() {
		ch.sync(&snap)
	}
	b.mu.Lock()
	o := b.tx
	b.mu.Unlock()
	if o != nil {
		if !snap.Connected {
			b.releaseTX(o.c, "radio connection lost")
		} else if _, ok := snap.SliceByLetter(o.c.ch.cfg.Slice); !ok {
			b.releaseTX(o.c, "transmitting slice was closed")
		}
	}
	b.notifyUI()
}

// resyncOnError returns a reply callback that restores the real state on failure.
func (b *Bridge) resyncOnError(ch *Channel) func(uint32, string) {
	return func(code uint32, _ string) {
		if code != 0 {
			ch.resync()
		}
	}
}

// ---- transmit ----

func isDigital(mode string) bool {
	m := strings.ToUpper(mode)
	return m == "DIGU" || m == "DIGL" || m == "RTTY"
}

func (b *Bridge) isTXOwner(c *client) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.tx != nil && b.tx.c == c
}

// requestTX keys the transmitter for client c on its channel's slice.
func (b *Bridge) requestTX(c *client, tciAudio, tune bool) error {
	snap := b.radio.Snapshot()
	if !snap.Connected {
		return errors.New("radio not connected")
	}
	sl, ok := snap.SliceByLetter(c.ch.cfg.Slice)
	if !ok {
		return fmt.Errorf("Slice %s is not open", c.ch.cfg.Slice)
	}
	if snap.TX.Inhibit {
		return errors.New("transmit is inhibited on the radio")
	}
	if !snap.Interlock.TxAllowed {
		return fmt.Errorf("radio reports transmit not allowed (%s)", snap.Interlock.Reason)
	}
	b.mu.Lock()
	if b.tx != nil && b.tx.c != c {
		other := b.tx.c
		b.mu.Unlock()
		return fmt.Errorf("client #%d on Slice %s is already transmitting", other.id, other.ch.cfg.Slice)
	}
	now := time.Now()
	o := b.tx
	if o == nil {
		o = &txOwner{c: c, sliceIdx: sl.Index, since: now}
		b.tx = o
	}
	o.tciAudio, o.tune = tciAudio, tune
	o.lastAudio, o.outstanding = now, 0
	var setDAX, openRender bool
	if tciAudio {
		if b.render == nil {
			b.fifo.Reset()
			openRender = true
		}
		if !isDigital(sl.Mode) && !snap.TX.DAX && !o.restoreDAX {
			o.restoreDAX, setDAX = true, true
		}
	}
	b.mu.Unlock()

	// Open the TX audio sink outside the lock: it may talk to the radio or
	// a sound device, and nothing else may wait on b.mu meanwhile.
	if openRender {
		r := b.newRender(b.pullTX)
		b.mu.Lock()
		if b.tx == o && b.render == nil {
			b.render, r = r, nil
		}
		b.mu.Unlock()
		if r != nil { // released (or replaced) while opening
			r.Stop()
		}
	}

	if !sl.TX {
		b.lg.Info("making Slice %s the TX slice", sl.Letter)
		b.radio.SetSlice(sl.Index, "tx=1")
	}
	if setDAX {
		b.lg.Info("Slice %s is in %s: switching TX audio source to DAX while TCI transmits", sl.Letter, sl.Mode)
		b.radio.SetTransmit("dax=1")
	}
	src := "radio audio (mic)"
	if tciAudio {
		src = "TCI audio"
	}
	if tune {
		b.lg.Info("client #%d TUNE on Slice %s", c.id, sl.Letter)
		b.radio.TuneTX(true)
	} else {
		b.lg.Info("client #%d keyed Slice %s using %s", c.id, sl.Letter, src)
		b.radio.Xmit(true)
	}
	b.notifyUI()
	return nil
}

// pullTX feeds the TX audio sink: FIFO audio with the TX gain applied, and
// the peak recorded for the TX meter.
func (b *Bridge) pullTX(out []float32) {
	b.fifo.Read(out)
	g := dbToGain(float64(b.cfg.Get().TXGainDB))
	var peak float32
	for i, v := range out {
		v *= g
		out[i] = v
		if v < 0 {
			v = -v
		}
		if v > peak {
			peak = v
		}
	}
	for {
		old := b.txPeak.Load()
		if peak <= math.Float32frombits(old) || b.txPeak.CompareAndSwap(old, math.Float32bits(peak)) {
			return
		}
	}
}

// releaseTX unkeys if c is the current owner.
func (b *Bridge) releaseTX(c *client, reason string) {
	b.mu.Lock()
	o := b.tx
	if o == nil || o.c != c {
		b.mu.Unlock()
		return
	}
	b.tx = nil
	r := b.render
	b.render = nil
	b.mu.Unlock()

	if o.tune {
		b.radio.TuneTX(false)
	} else {
		b.radio.Xmit(false)
	}
	if o.restoreDAX {
		b.radio.SetTransmit("dax=0")
	}
	if r != nil {
		go r.Stop()
	}
	stats := ""
	if o.tciAudio {
		stats = fmt.Sprintf(", %d TX_CHRONO sent, %d audio blocks received", o.chronos, o.blocks)
	}
	b.lg.Info("unkeyed Slice %s (client #%d): %s, keyed %.1f s%s", c.ch.cfg.Slice, c.id, reason, time.Since(o.since).Seconds(), stats)
	c.ch.pinAndBroadcast(nil, "trx", false, func(v *view) { v.TRX, v.Tune = false, false })
	b.notifyUI()
}

// unkeyRequest handles TRX/TUNE false from any client. Any TCI client may
// unkey TCI transmit; transmit started elsewhere (SmartSDR MOX) is left alone.
func (b *Bridge) unkeyRequest(c *client, reason string) {
	b.mu.Lock()
	o := b.tx
	b.mu.Unlock()
	if o == nil {
		return
	}
	if o.c != c {
		reason = fmt.Sprintf("unkeyed by client #%d", c.id)
	}
	b.releaseTX(o.c, reason)
}

func (b *Bridge) releaseAll(reason string) {
	b.mu.Lock()
	o := b.tx
	b.mu.Unlock()
	if o != nil {
		b.releaseTX(o.c, reason)
	}
}

// txAudio accepts a TX_AUDIO_STREAM frame from a client.
func (b *Bridge) txAudio(c *client, h tci.Header, s []float32) {
	b.mu.Lock()
	o := b.tx
	ok := o != nil && o.c == c && o.tciAudio
	b.mu.Unlock()
	if !ok {
		return
	}
	st := c.toStereo48k(h, s)
	b.fifo.Write(st)
	b.mu.Lock()
	if b.tx == o {
		o.blocks++
		o.lastAudio = time.Now()
		o.outstanding -= len(st) / 2
		if o.outstanding < 0 {
			o.outstanding = 0
		}
	}
	b.mu.Unlock()
}

// txLoop paces TX_CHRONO requests from the TX FIFO level and enforces the
// safety unkey when a transmitting client stops sending audio.
func (b *Bridge) txLoop() {
	t := time.NewTicker(5 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-b.quit:
			return
		case <-t.C:
		}
		timeout := time.Duration(b.cfg.Get().UnkeyTimeoutMs) * time.Millisecond
		b.mu.Lock()
		o := b.tx
		if o == nil || !o.tciAudio {
			b.mu.Unlock()
			continue
		}
		if silent := time.Since(o.lastAudio); silent > timeout {
			// If the FIFO is still full the client was fine; the DAX TX
			// stream to the radio is not taking audio.
			stalled := o.blocks > 0 && b.fifo.Frames() >= o.c.txBufferMs()*audio.Rate/1000
			b.mu.Unlock()
			if stalled {
				b.lg.Error("SAFETY: TX audio is not being sent to the radio (DAX TX stream stalled)")
				b.releaseTX(o.c, "DAX TX stream stalled (safety unkey)")
			} else {
				b.lg.Warn("SAFETY: no TX audio from client #%d for %d ms", o.c.id, silent.Milliseconds())
				b.releaseTX(o.c, "TX audio stopped (safety unkey)")
			}
			continue
		}
		// Forget requests the client never answered so we ask again.
		if o.outstanding > 0 && time.Since(o.lastChrono) > 250*time.Millisecond && time.Since(o.lastAudio) > 250*time.Millisecond {
			o.outstanding = 0
		}
		c := o.c
		level := b.fifo.Frames() + o.outstanding
		b.mu.Unlock()

		target := c.txBufferMs() * audio.Rate / 1000
		if level >= target {
			continue
		}
		frame, f48 := c.chronoFrame()
		if !c.sendAudio(frame) {
			continue
		}
		b.mu.Lock()
		if b.tx == o {
			o.chronos++
			o.outstanding += f48
			o.lastChrono = time.Now()
		}
		b.mu.Unlock()
	}
}

// ---- UI ----

func (b *Bridge) notifyUI() { b.uiDirty.Store(true) }

func (b *Bridge) uiLoop() {
	t := time.NewTicker(250 * time.Millisecond)
	defer t.Stop()
	n := 0
	for {
		select {
		case <-b.quit:
			return
		case <-t.C:
		}
		n++
		// Push on change, and at least every second for level meters.
		if b.uiDirty.Swap(false) || n%4 == 0 {
			if b.onUI != nil {
				b.onUI()
			}
		}
	}
}

type Status struct {
	Radio    flex.Snapshot    `json:"radio"`
	Radios   []flex.RadioInfo `json:"radios"`
	Channels []ChannelStatus  `json:"channels"`
	TXOwner  string           `json:"tx_owner"`
	TXLevel  float32          `json:"tx_level"` // peak TX audio since the last status
	Config   config.Config    `json:"config"`
}

func (b *Bridge) Status() Status {
	st := Status{Radio: b.radio.Snapshot(), Radios: b.disc.List(), Config: b.cfg.Get()}
	st.TXLevel = math.Float32frombits(b.txPeak.Swap(0))
	for _, ch := range b.channelList() {
		st.Channels = append(st.Channels, ch.status())
	}
	b.mu.Lock()
	if b.tx != nil {
		kind := "TX"
		if b.tx.tune {
			kind = "TUNE"
		}
		st.TXOwner = fmt.Sprintf("%s: Slice %s, client #%d", kind, b.tx.c.ch.cfg.Slice, b.tx.c.id)
	}
	b.mu.Unlock()
	return st
}
