package bridge

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"github.com/va3mw/FlexTCI/internal/audio"
	"github.com/va3mw/FlexTCI/internal/tci"
)

type outMsg struct {
	text string
	bin  []byte
}

// client is one TCI WebSocket connection on a Channel.
type client struct {
	ch     *Channel
	conn   *websocket.Conn
	id     int
	remote string

	out       chan outMsg
	done      chan struct{}
	closeOnce sync.Once
	reason    atomic.Value // string

	audioOn atomic.Bool

	mu         sync.Mutex
	rate       int
	format     uint32
	chans      int
	samples    int
	samplesSet bool
	txBufMs    int
	rxRs       *audio.Resampler
	rxAcc      []float32
	txRs       *audio.Resampler
	txRsKey    int
	dropped    int
	ignored    map[string]bool
}

func newClient(ch *Channel, conn *websocket.Conn, id int) *client {
	c := &client{
		ch: ch, conn: conn, id: id, remote: conn.RemoteAddr().String(),
		out: make(chan outMsg, 512), done: make(chan struct{}),
		rate: 48000, format: tci.FmtFloat32, chans: 2, samples: 2048, txBufMs: 50,
		ignored: map[string]bool{},
	}
	c.rxRs = audio.NewResampler(audio.Rate, c.rate, 2)
	return c
}

// run reads from the connection until it closes. The writer runs alongside.
func (c *client) run() {
	go c.writer()
	c.conn.SetReadLimit(1 << 20)
	const readTimeout = 10 * time.Second
	c.conn.SetReadDeadline(time.Now().Add(readTimeout))
	c.conn.SetPongHandler(func(string) error {
		c.conn.SetReadDeadline(time.Now().Add(readTimeout))
		return nil
	})
	for {
		mt, data, err := c.conn.ReadMessage()
		if err != nil {
			if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				c.close("closed by client")
			} else {
				c.close(err.Error())
			}
			return
		}
		c.conn.SetReadDeadline(time.Now().Add(readTimeout))
		switch mt {
		case websocket.TextMessage:
			c.handleText(string(data))
		case websocket.BinaryMessage:
			c.handleBinary(data)
		}
	}
}

func (c *client) writer() {
	ping := time.NewTicker(2 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-c.done:
			return
		case m := <-c.out:
			c.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			var err error
			if m.bin != nil {
				err = c.conn.WriteMessage(websocket.BinaryMessage, m.bin)
			} else {
				err = c.conn.WriteMessage(websocket.TextMessage, []byte(m.text))
			}
			if err != nil {
				c.close("write failed: " + err.Error())
				return
			}
		case <-ping.C:
			if err := c.conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(2*time.Second)); err != nil {
				c.close("ping failed: " + err.Error())
				return
			}
		}
	}
}

func (c *client) close(reason string) {
	c.closeOnce.Do(func() {
		c.reason.Store(reason)
		close(c.done)
		c.conn.Close()
	})
}

func (c *client) closeReason() string {
	if r, ok := c.reason.Load().(string); ok {
		return r
	}
	return "unknown"
}

// sendText queues text commands, one per WebSocket frame.
func (c *client) sendText(lines ...string) {
	for _, l := range lines {
		select {
		case <-c.done:
			return
		case c.out <- outMsg{text: l}:
			c.ch.lg.Traffic("#%d -> %s", c.id, l)
		default:
			c.close("client is not reading (send queue full)")
			return
		}
	}
}

// sendAudio queues a binary frame, dropping it if the client is falling behind.
func (c *client) sendAudio(b []byte) bool {
	if len(c.out) > cap(c.out)/2 {
		c.mu.Lock()
		c.dropped++
		d := c.dropped
		c.mu.Unlock()
		if d == 1 || d%100 == 0 {
			c.ch.lg.Warn("#%d falling behind, dropped %d audio frames", c.id, d)
		}
		return false
	}
	select {
	case c.out <- outMsg{bin: b}:
		return true
	default:
		return false
	}
}

func (c *client) sendInit() {
	c.sendText(c.ch.initLines()...)
}

// ---- RX audio ----

func (c *client) pushRX(s []float32) {
	c.mu.Lock()
	x := c.rxRs.Process(s)
	if c.chans == 1 {
		m := make([]float32, len(x)/2)
		for i := range m {
			m[i] = (x[2*i] + x[2*i+1]) / 2
		}
		x = m
	}
	c.rxAcc = append(c.rxAcc, x...)
	n := c.samples - c.samples%c.chans
	h := tci.Header{SampleRate: uint32(c.rate), Format: c.format, Type: tci.StreamRXAudio, Channels: uint32(c.chans)}
	var frames [][]byte
	for len(c.rxAcc) >= n {
		frames = append(frames, tci.Encode(h, c.rxAcc[:n]))
		c.rxAcc = c.rxAcc[n:]
	}
	if len(c.rxAcc) == 0 {
		c.rxAcc = nil
	}
	c.mu.Unlock()
	for _, f := range frames {
		c.sendAudio(f)
	}
}

// ---- incoming ----

func (c *client) handleBinary(data []byte) {
	h, err := tci.ReadHeader(data)
	if err != nil {
		return
	}
	if h.Type != tci.StreamTXAudio {
		return
	}
	c.ch.b.txAudio(c, h, tci.Decode(h, data[tci.HeaderSize:]))
}

// toStereo48k converts client TX audio to the stereo 48 kHz used internally.
func (c *client) toStereo48k(h tci.Header, s []float32) []float32 {
	c.mu.Lock()
	defer c.mu.Unlock()
	chans := int(h.Channels)
	if chans != 1 && chans != 2 {
		chans = c.chans
	}
	// Only TCI rates are accepted: an arbitrary header rate (say 1 Hz) would
	// make the resampler allocate hundreds of MB per packet.
	rate := int(h.SampleRate)
	if !validRate(rate) {
		rate = c.rate
	}
	if chans == 1 {
		st := make([]float32, len(s)*2)
		for i, v := range s {
			st[2*i], st[2*i+1] = v, v
		}
		s = st
	}
	if rate == audio.Rate {
		return s
	}
	if c.txRs == nil || c.txRsKey != rate {
		c.txRs, c.txRsKey = audio.NewResampler(rate, audio.Rate, 2), rate
	}
	return c.txRs.Process(s)
}

func validRate(r int) bool { return r == 8000 || r == 12000 || r == 24000 || r == 48000 }

// chronoFrame is the TX_CHRONO request for one block in the client's format.
func (c *client) chronoFrame() (frame []byte, frames48k int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := c.samples - c.samples%c.chans
	h := tci.Header{SampleRate: uint32(c.rate), Format: c.format, Type: tci.StreamTXChrono, Channels: uint32(c.chans)}
	frames48k = n / c.chans * audio.Rate / c.rate
	return tci.Encode(h, make([]float32, n)), frames48k
}

func (c *client) txBufferMs() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.txBufMs
}

func (c *client) handleText(frame string) {
	if c.ch.lg.TrafficOn() {
		s := strings.TrimSpace(frame)
		if len(s) > 400 { // a client may send up to 1 MB; keep the log usable
			s = s[:400] + fmt.Sprintf("… (%d bytes)", len(frame))
		}
		c.ch.lg.Traffic("#%d <- %s", c.id, s)
	}
	for _, cmd := range tci.Parse(frame) {
		c.dispatch(cmd)
	}
}

func atoi(s string) (int, bool) {
	v, err := strconv.Atoi(s)
	return v, err == nil
}

func (c *client) dispatch(cmd tci.Command) {
	ch, b := c.ch, c.ch.b
	a := cmd.Args
	v := ch.currentView()
	// needSlice guards set commands; it logs and returns -1 when the slice is closed.
	needSlice := func() int {
		idx := ch.sliceIndex()
		if idx < 0 {
			ch.lg.Warn("#%d %s ignored: Slice %s is not open", c.id, cmd.Raw, ch.cfg.Slice)
		}
		return idx
	}
	switch cmd.Name {
	case "vfo":
		if len(a) < 2 {
			return
		}
		if len(a) >= 3 {
			hz, err := strconv.ParseInt(a[2], 10, 64)
			if err != nil || hz <= 0 {
				return
			}
			if a[1] != "0" {
				ch.lg.Debug("#%d VFO B set not supported (split not mapped yet); echoing VFO A", c.id)
			} else if idx := needSlice(); idx >= 0 {
				b.radio.Tune(idx, hz)
				ch.pinAndBroadcast(c, "freq", hz, func(v *view) { v.Freq = hz }, freqLines(hz)...)
				return
			}
		}
		c.sendText(tci.Fmt("vfo", 0, a[1], ch.currentView().Freq))
	case "dds":
		// The panadapter centre is not exposed separately: DDS and VFO A
		// both mean the slice frequency. Some clients tune with DDS (Hamlib
		// sends DDS then VFO), so a DDS set must tune or they wait.
		if len(a) >= 2 {
			if hz, err := strconv.ParseInt(a[1], 10, 64); err == nil && hz > 0 {
				if idx := needSlice(); idx >= 0 {
					if hz != v.Freq {
						b.radio.Tune(idx, hz)
					}
					ch.pinAndBroadcast(c, "freq", hz, func(v *view) { v.Freq = hz }, freqLines(hz)...)
					return
				}
			}
		}
		c.sendText(tci.Fmt("dds", 0, ch.currentView().Freq))
	case "if":
		if len(a) >= 2 {
			c.sendText(tci.Fmt("if", 0, a[1], 0))
		}
	case "modulation":
		if len(a) >= 2 {
			mode, ok := tci.TCIToFlex(a[1])
			if !ok {
				ch.lg.Warn("#%d unsupported mode %q", c.id, a[1])
			} else if idx := needSlice(); idx >= 0 {
				name := tci.FlexToTCI(mode)
				b.radio.Command("slice set "+strconv.Itoa(idx)+" mode="+mode, b.resyncOnError(ch))
				ch.pinAndBroadcast(c, "mode", name, func(v *view) { v.Mode = name }, tci.Fmt("modulation", 0, name))
				return
			}
		}
		c.sendText(tci.Fmt("modulation", 0, ch.currentView().Mode))
	case "rx_filter_band":
		if len(a) >= 3 {
			lo, ok1 := atoi(a[1])
			hi, ok2 := atoi(a[2])
			if idx := needSlice(); ok1 && ok2 && idx >= 0 {
				b.radio.SetFilter(idx, lo, hi)
				return // the radio's status update is echoed to everyone
			}
		}
		cur := ch.currentView()
		c.sendText(tci.Fmt("rx_filter_band", 0, cur.FLo, cur.FHi))
	case "trx":
		if len(a) >= 2 {
			on, ok := tci.ParseBool(a[1])
			if !ok {
				return
			}
			src := ""
			if len(a) >= 3 {
				src = strings.ToLower(a[2])
			}
			if on {
				if err := b.requestTX(c, src == "tci", false); err != nil {
					ch.lg.Warn("#%d TX refused: %v", c.id, err)
					c.sendText(tci.Fmt("trx", 0, false))
					return
				}
				ch.pinAndBroadcast(c, "trx", true, func(v *view) { v.TRX = true }, tci.Fmt("trx", 0, true))
			} else {
				b.unkeyRequest(c, "client unkeyed")
				ch.pinAndBroadcast(c, "trx", false, func(v *view) { v.TRX = false }, tci.Fmt("trx", 0, false))
			}
			return
		}
		c.sendText(tci.Fmt("trx", 0, v.TRX))
	case "tune":
		if len(a) >= 2 {
			on, ok := tci.ParseBool(a[1])
			if !ok {
				return
			}
			if on {
				if err := b.requestTX(c, false, true); err != nil {
					ch.lg.Warn("#%d TUNE refused: %v", c.id, err)
					c.sendText(tci.Fmt("tune", 0, false))
					return
				}
				ch.pinAndBroadcast(c, "tune", true, func(v *view) { v.Tune = true }, tci.Fmt("tune", 0, true))
			} else {
				b.unkeyRequest(c, "client ended tune")
				ch.pinAndBroadcast(c, "tune", false, func(v *view) { v.Tune = false }, tci.Fmt("tune", 0, false))
			}
			return
		}
		c.sendText(tci.Fmt("tune", 0, v.Tune))
	case "drive", "tune_drive":
		if len(a) >= 2 {
			if p, ok := atoi(a[1]); ok && p >= 0 && p <= 100 {
				key := "rfpower"
				if cmd.Name == "tune_drive" {
					key = "tunepower"
				}
				b.radio.SetTransmit(key + "=" + strconv.Itoa(p))
				if cmd.Name == "drive" {
					ch.pinAndBroadcast(c, "drive", p, func(v *view) { v.Drive = p }, tci.Fmt("drive", 0, p))
				} else {
					ch.pinAndBroadcast(c, "tune_drive", p, func(v *view) { v.TuneDrive = p }, tci.Fmt("tune_drive", 0, p))
				}
				return
			}
		}
		val := v.Drive
		if cmd.Name == "tune_drive" {
			val = v.TuneDrive
		}
		c.sendText(tci.Fmt(cmd.Name, 0, val))
	case "tx_enable":
		c.sendText(tci.Fmt("tx_enable", 0, v.TXEnable))
	case "split_enable":
		if len(a) >= 2 {
			if on, _ := tci.ParseBool(a[1]); on {
				ch.lg.Warn("#%d split is not supported yet; set the client's split to None or Fake It", c.id)
			}
		}
		c.sendText(tci.Fmt("split_enable", 0, false))
	case "rit_enable", "xit_enable":
		key := strings.TrimSuffix(cmd.Name, "_enable") + "_on"
		if len(a) >= 2 {
			if on, ok := tci.ParseBool(a[1]); ok {
				if idx := needSlice(); idx >= 0 {
					b.radio.SetSlice(idx, key+"="+flexBool(on))
					return
				}
			}
		}
		cur := v.RITOn
		if cmd.Name == "xit_enable" {
			cur = v.XITOn
		}
		c.sendText(tci.Fmt(cmd.Name, 0, cur))
	case "rit_offset", "xit_offset":
		key := strings.TrimSuffix(cmd.Name, "_offset") + "_freq"
		if len(a) >= 2 {
			if hz, ok := atoi(a[1]); ok {
				if idx := needSlice(); idx >= 0 {
					b.radio.SetSlice(idx, key+"="+strconv.Itoa(hz))
					return
				}
			}
		}
		cur := v.RIT
		if cmd.Name == "xit_offset" {
			cur = v.XIT
		}
		c.sendText(tci.Fmt(cmd.Name, 0, cur))
	case "audio_start":
		if !c.audioOn.Swap(true) {
			c.mu.Lock()
			ch.lg.Info("#%d audio start (%d Hz, %s, %d ch, %d samples/packet)", c.id, c.rate, tci.FormatName(c.format), c.chans, c.samples)
			c.mu.Unlock()
		}
		// WSJT-X waits for this echo; without it reports "TCI Audio could not be switched on".
		c.sendText(tci.Fmt("audio_start", 0))
		ch.updateCapture()
	case "audio_stop":
		if c.audioOn.Swap(false) {
			ch.lg.Info("#%d audio stop", c.id)
		}
		c.sendText(tci.Fmt("audio_stop", 0))
		ch.updateCapture()
	case "audio_samplerate":
		if len(a) >= 1 {
			if r, ok := atoi(a[0]); ok && validRate(r) {
				c.mu.Lock()
				c.rate = r
				if !c.samplesSet {
					c.samples = map[int]int{48000: 2048, 24000: 1024, 12000: 512, 8000: 256}[r]
				}
				c.rxRs = audio.NewResampler(audio.Rate, r, 2)
				c.rxAcc = nil
				c.mu.Unlock()
				c.sendText(tci.Fmt("audio_samplerate", r))
			} else {
				ch.lg.Warn("#%d unsupported audio sample rate %q", c.id, a[0])
			}
		}
	case "audio_stream_sample_type":
		if len(a) >= 1 {
			if f, ok := tci.FormatByName(a[0]); ok {
				c.mu.Lock()
				c.format = f
				c.mu.Unlock()
				c.sendText(tci.Fmt(cmd.Name, tci.FormatName(f)))
			}
		}
	case "audio_stream_channels":
		if len(a) >= 1 {
			if n, ok := atoi(a[0]); ok && (n == 1 || n == 2) {
				c.mu.Lock()
				c.chans = n
				c.rxAcc = nil
				c.mu.Unlock()
				c.sendText(tci.Fmt(cmd.Name, n))
			}
		}
	case "audio_stream_samples":
		if len(a) >= 1 {
			if n, ok := atoi(a[0]); ok && n >= 100 && n <= 2048 {
				c.mu.Lock()
				c.samples, c.samplesSet = n, true
				c.rxAcc = nil
				c.mu.Unlock()
				c.sendText(tci.Fmt(cmd.Name, n))
			}
		}
	case "tx_stream_audio_buffering":
		if len(a) >= 1 {
			if n, ok := atoi(a[0]); ok && n >= 50 && n <= 500 {
				c.mu.Lock()
				c.txBufMs = n
				c.mu.Unlock()
				c.sendText(tci.Fmt(cmd.Name, n))
			}
		}
	case "start", "stop":
		ch.lg.Debug("#%d %s ignored (radio power is not controlled over TCI)", c.id, cmd.Name)
	default:
		if !c.ignored[cmd.Name] && len(c.ignored) < 64 { // bounded: names come from the client
			c.ignored[cmd.Name] = true
			ch.lg.Info("#%d ignoring unsupported command %s (logged once)", c.id, strings.ToUpper(cmd.Name))
		}
	}
}

func flexBool(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// ClientStatus is shown in the UI.
type ClientStatus struct {
	ID       int    `json:"id"`
	Remote   string `json:"remote"`
	Audio    bool   `json:"audio"`
	Rate     int    `json:"rate"`
	Format   string `json:"format"`
	Channels int    `json:"channels"`
	TX       bool   `json:"tx"`
}

// status must not be called with the bridge lock held (lock order is
// bridge -> client).
func (c *client) status() ClientStatus {
	tx := c.ch.b.isTXOwner(c)
	c.mu.Lock()
	defer c.mu.Unlock()
	return ClientStatus{
		ID: c.id, Remote: c.remote, Audio: c.audioOn.Load(), Rate: c.rate,
		Format: tci.FormatName(c.format), Channels: c.chans, TX: tx,
	}
}
