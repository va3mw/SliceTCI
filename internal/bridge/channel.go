package bridge

import (
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gorilla/websocket"

	"github.com/va3mw/FlexTCI/internal/config"
	"github.com/va3mw/FlexTCI/internal/flex"
	"github.com/va3mw/FlexTCI/internal/logx"
	"github.com/va3mw/FlexTCI/internal/tci"
)

// Channel is one TCI listener (one port) bound to one slice letter and one
// DAX RX audio device.
type Channel struct {
	b   *Bridge
	idx int
	cfg config.Channel
	lg  *logx.Source

	ln  net.Listener
	srv *http.Server

	mu        sync.Mutex
	clients   map[*client]struct{}
	nextID    int
	last      view
	pins      map[string]pin
	capture   stopper
	listenErr string
	rxPeak    float32
	rxGain    atomic.Uint32 // float32 bits, linear; direct path only
	daxSetFor int           // slice index we last set dax= on
}

// setRXGain changes the RX gain live. ch.cfg is read without a lock by other
// goroutines and is never modified after creation; the gain lives only in
// the atomic.
func (ch *Channel) setRXGain(db int) {
	g := math.Float32bits(dbToGain(float64(db)))
	if ch.rxGain.Swap(g) != g {
		ch.lg.Info("RX gain %d dB", db)
	}
}

func (ch *Channel) rxGainLinear() float32 { return math.Float32frombits(ch.rxGain.Load()) }

func newChannel(b *Bridge, idx int, cfg config.Channel) *Channel {
	ch := &Channel{
		b:         b,
		idx:       idx,
		cfg:       cfg,
		lg:        b.log.Src(fmt.Sprintf("tci-%s", cfg.Slice)),
		clients:   map[*client]struct{}{},
		pins:      map[string]pin{},
		last:      view{SliceIdx: -1},
		daxSetFor: -1,
	}
	ch.rxGain.Store(math.Float32bits(dbToGain(float64(cfg.RXGain()))))
	return ch
}

var upgrader = websocket.Upgrader{
	ReadBufferSize:  64 * 1024,
	WriteBufferSize: 64 * 1024,
	CheckOrigin:     allowedOrigin,
}

// allowedOrigin blocks web pages. Binding to localhost does not stop a
// browser: any site the operator visits can open ws://127.0.0.1:50001 and
// key the transmitter. Browsers always send an Origin header; native TCI
// clients (WSJT-X, Hamlib, loggers) send none, or a localhost one.
func allowedOrigin(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		return true
	}
	u, err := url.Parse(o)
	if err != nil {
		return false
	}
	switch u.Hostname() {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}

func (ch *Channel) start() {
	addr := fmt.Sprintf("127.0.0.1:%d", ch.cfg.Port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		msg := err.Error()
		if errors.Is(err, syscall.Errno(10048)) { // WSAEADDRINUSE
			msg = fmt.Sprintf("port %d is already in use by another program (ExpertSDR3, AetherSDR or another TCI server?). Pick a different port.", ch.cfg.Port)
		}
		ch.mu.Lock()
		ch.listenErr = msg
		ch.mu.Unlock()
		ch.lg.Error("cannot listen on %s: %s", addr, msg)
		return
	}
	ch.ln = ln
	ch.srv = &http.Server{Handler: http.HandlerFunc(ch.serveWS), ReadHeaderTimeout: 5 * time.Second}
	go ch.srv.Serve(ln)
	// Also listen on IPv6 loopback so "localhost" works when a client
	// resolves it to ::1 first. Optional: IPv6 may be disabled.
	where := "127.0.0.1"
	if ln6, err := net.Listen("tcp", fmt.Sprintf("[::1]:%d", ch.cfg.Port)); err == nil {
		go ch.srv.Serve(ln6)
		where = "127.0.0.1 and [::1]"
	} else {
		ch.lg.Debug("no IPv6 loopback listener on port %d: %v", ch.cfg.Port, err)
	}
	ch.lg.Info("listening on port %d (%s) for Slice %s (DAX %d)", ch.cfg.Port, where, ch.cfg.Slice, ch.cfg.DAX)
}

func (ch *Channel) stop() {
	if ch.srv != nil {
		ch.srv.Close()
	}
	ch.mu.Lock()
	cl := ch.clientList()
	capt := ch.capture
	ch.capture = nil
	ch.mu.Unlock()
	for _, c := range cl {
		c.close("channel stopped")
	}
	if capt != nil {
		capt.Stop()
	}
	ch.lg.Info("stopped listener on port %d", ch.cfg.Port)
}

func (ch *Channel) serveWS(w http.ResponseWriter, r *http.Request) {
	if !allowedOrigin(r) {
		ch.lg.Warn("refused connection from web page %q (browsers may not control the radio)", r.Header.Get("Origin"))
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		ch.lg.Warn("websocket upgrade from %s failed: %v", r.RemoteAddr, err)
		return
	}
	ch.mu.Lock()
	ch.nextID++
	c := newClient(ch, conn, ch.nextID)
	ch.clients[c] = struct{}{}
	n := len(ch.clients)
	ch.mu.Unlock()
	ch.lg.Info("client #%d connected from %s (%d on this port)", c.id, r.RemoteAddr, n)
	c.sendInit()
	ch.ensureSliceDAX()
	ch.b.notifyUI()
	c.run() // blocks until the connection ends
	ch.removeClient(c)
}

func (ch *Channel) removeClient(c *client) {
	ch.mu.Lock()
	delete(ch.clients, c)
	n := len(ch.clients)
	if n == 0 {
		ch.daxSetFor = -1
	}
	ch.mu.Unlock()
	ch.lg.Info("client #%d disconnected: %s (%d left)", c.id, c.closeReason(), n)
	ch.b.releaseTX(c, "client disconnected")
	ch.updateCapture()
	ch.b.notifyUI()
}

func (ch *Channel) clientList() []*client {
	out := make([]*client, 0, len(ch.clients))
	for c := range ch.clients {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].id < out[j].id })
	return out
}

// sync recomputes the view from the radio snapshot and pushes differences.
func (ch *Channel) sync(s *flex.Snapshot) {
	cur := computeView(s, ch.cfg.Slice)
	ch.mu.Lock()
	applyPins(&cur, ch.pins)
	old := ch.last
	ch.last = cur
	lines := diff(old, cur, false)
	cl := ch.clientList()
	ch.mu.Unlock()

	if old.Present != cur.Present {
		if cur.Present {
			ch.lg.Info("Slice %s is available (radio slice %d)", ch.cfg.Slice, cur.SliceIdx)
		} else if s.Connected {
			ch.lg.Warn("Slice %s is not open on station \"%s\"; TX disabled for this channel", ch.cfg.Slice, s.BoundStation)
		}
	}
	if cur.Present {
		ch.ensureSliceDAX()
	}
	if len(lines) == 0 {
		return
	}
	for _, c := range cl {
		c.sendText(lines...)
	}
}

// ensureSliceDAX sets the slice's DAX channel to match this channel's
// assignment once per slice while clients are connected. It does not fight a
// later manual change in SmartSDR; reconnecting the client re-applies it.
func (ch *Channel) ensureSliceDAX() {
	if !ch.b.cfg.Get().SetSliceDAX {
		return
	}
	snap := ch.b.radio.Snapshot()
	sl, ok := snap.SliceByLetter(ch.cfg.Slice)
	ch.mu.Lock()
	if !ok || len(ch.clients) == 0 {
		ch.daxSetFor = -1
		ch.mu.Unlock()
		return
	}
	if ch.daxSetFor == sl.Index {
		ch.mu.Unlock()
		return
	}
	ch.daxSetFor = sl.Index
	ch.mu.Unlock()
	if sl.DAX == ch.cfg.DAX {
		return
	}
	ch.lg.Info("setting Slice %s DAX channel %d -> %d", ch.cfg.Slice, sl.DAX, ch.cfg.DAX)
	ch.b.radio.SetSlice(sl.Index, fmt.Sprintf("dax=%d", ch.cfg.DAX))
}

// pinAndBroadcast records an optimistic value and pushes any change to every
// client. The originating client always gets echo, even when nothing changed:
// clients (WSJT-X) wait for the echo of every set they send.
func (ch *Channel) pinAndBroadcast(origin *client, key string, val any, apply func(v *view), echo ...string) {
	ch.mu.Lock()
	ch.pins[key] = pin{val: val, until: time.Now().Add(pinHold)}
	old := ch.last
	apply(&ch.last)
	lines := diff(old, ch.last, false)
	cl := ch.clientList()
	ch.mu.Unlock()
	for _, c := range cl {
		c.sendText(lines...)
	}
	if len(lines) == 0 && origin != nil {
		origin.sendText(echo...)
	}
}

// resync drops pins and pushes the radio's real state (used after a rejected command).
func (ch *Channel) resync() {
	ch.mu.Lock()
	ch.pins = map[string]pin{}
	ch.mu.Unlock()
	s := ch.b.radio.Snapshot()
	ch.sync(&s)
}

func (ch *Channel) currentView() view {
	ch.mu.Lock()
	defer ch.mu.Unlock()
	return ch.last
}

// updateCapture starts or stops the DAX RX capture depending on demand.
func (ch *Channel) updateCapture() {
	ch.mu.Lock()
	need := false
	for c := range ch.clients {
		if c.audioOn.Load() {
			need = true
		}
	}
	var stop stopper
	if need && ch.capture == nil {
		ch.lg.Info("starting RX audio from the radio (DAX channel %d)", ch.cfg.DAX)
		ch.capture = startDirectRX(ch.b.radio, ch.lg, ch.cfg.DAX, ch.rxGainLinear, ch.onRX)
	} else if !need && ch.capture != nil {
		stop = ch.capture
		ch.capture = nil
	}
	ch.mu.Unlock()
	if stop != nil {
		ch.lg.Info("no clients want RX audio; closing the RX audio stream")
		go stop.Stop()
	}
}

func (ch *Channel) onRX(s []float32) {
	var peak float32
	for _, v := range s {
		if a := float32(math.Abs(float64(v))); a > peak {
			peak = a
		}
	}
	ch.mu.Lock()
	if peak > ch.rxPeak {
		ch.rxPeak = peak
	}
	var targets []*client
	for c := range ch.clients {
		if c.audioOn.Load() {
			targets = append(targets, c)
		}
	}
	ch.mu.Unlock()
	for _, c := range targets {
		c.pushRX(s)
	}
}

// ChannelStatus is shown in the UI.
type ChannelStatus struct {
	Index     int            `json:"index"`
	Slice     string         `json:"slice"`
	Port      int            `json:"port"`
	DAX       int            `json:"dax"`
	Listening bool           `json:"listening"`
	Error     string         `json:"error"`
	Present   bool           `json:"present"`
	FreqHz    int64          `json:"freq_hz"`
	Mode      string         `json:"mode"`
	TRX       bool           `json:"trx"`
	TXEnable  bool           `json:"tx_enable"`
	RXLevel   float32        `json:"rx_level"`
	Audio     bool           `json:"audio"`
	Clients   []ClientStatus `json:"clients"`
}

func (ch *Channel) status() ChannelStatus {
	ch.mu.Lock()
	defer ch.mu.Unlock()
	st := ChannelStatus{
		Index: ch.idx, Slice: ch.cfg.Slice, Port: ch.cfg.Port, DAX: ch.cfg.DAX,
		Listening: ch.ln != nil, Error: ch.listenErr,
		Present: ch.last.Present, FreqHz: ch.last.Freq, Mode: ch.last.Mode, TRX: ch.last.TRX || ch.last.Tune,
		TXEnable: ch.last.TXEnable, RXLevel: ch.rxPeak, Audio: ch.capture != nil,
	}
	ch.rxPeak = 0
	for _, c := range ch.clientList() {
		st.Clients = append(st.Clients, c.status())
	}
	return st
}

// sliceIndex returns the radio slice index, or -1 if the slice is not open.
func (ch *Channel) sliceIndex() int {
	snap := ch.b.radio.Snapshot()
	if sl, ok := snap.SliceByLetter(ch.cfg.Slice); ok {
		return sl.Index
	}
	return -1
}

func (ch *Channel) initLines() []string {
	cfg := ch.b.cfg.Get()
	snap := ch.b.radio.Snapshot()
	device := snap.Model
	if device == "" {
		device = "FlexRadio"
	}
	lines := []string{
		tci.Fmt("protocol", cfg.ProtocolName, "2.0"),
		tci.Fmt("device", device),
		tci.Fmt("receive_only", false),
		tci.Fmt("trx_count", 1),
		tci.Fmt("channel_count", 2),
		tci.Fmt("vfo_limits", 10000, 54000000),
		tci.Fmt("if_limits", -24000, 24000),
	}
	modes := tci.ModeList
	if sl, ok := snap.SliceByLetter(ch.cfg.Slice); ok && len(sl.ModeList) > 0 {
		modes = tci.ModesFromFlex(sl.ModeList)
	}
	lines = append(lines, "modulations_list:"+strings.Join(modes, ",")+";")
	ch.mu.Lock()
	cur := ch.last
	ch.mu.Unlock()
	lines = append(lines, diff(cur, cur, true)...)
	lines = append(lines, "start;", "ready;")
	return lines
}
