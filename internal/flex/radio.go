package flex

import (
	"bufio"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/va3mw/SliceTCI/internal/logx"
)

// Slice mirrors the radio's slice status.
type Slice struct {
	Index        int      `json:"index"`
	Letter       string   `json:"letter"`
	InUse        bool     `json:"in_use"`
	FreqHz       int64    `json:"freq_hz"`
	Mode         string   `json:"mode"`
	FilterLo     int      `json:"filter_lo"`
	FilterHi     int      `json:"filter_hi"`
	DAX          int      `json:"dax"`
	TX           bool     `json:"tx"`
	ClientHandle uint32   `json:"client_handle"`
	RITOn        bool     `json:"rit_on"`
	RITFreq      int      `json:"rit_freq"`
	XITOn        bool     `json:"xit_on"`
	XITFreq      int      `json:"xit_freq"`
	ModeList     []string `json:"mode_list"`
	SampleRate   int      `json:"sample_rate"` // DAX audio rate of this slice
}

// Station is a GUI client (SmartSDR, Maestro, ...) connected to the radio.
type Station struct {
	Handle   uint32 `json:"handle"`
	ClientID string `json:"client_id"`
	Program  string `json:"program"`
	Name     string `json:"name"`
}

type TxState struct {
	RFPower   int  `json:"rf_power"`
	TunePower int  `json:"tune_power"`
	Tune      bool `json:"tune"`
	DAX       bool `json:"dax"`
	Inhibit   bool `json:"inhibit"`
}

type Interlock struct {
	State     string `json:"state"`
	TxAllowed bool   `json:"tx_allowed"`
	Reason    string `json:"reason"`
	TxHandle  uint32 `json:"tx_client_handle"` // client whose transmitter is keyed
}

// Transmitting reports whether the interlock says the radio is keyed.
func (i Interlock) Transmitting() bool {
	return i.State == "TRANSMITTING" || i.State == "PTT_REQUESTED"
}

// Snapshot is a consistent copy of the radio state.
type Snapshot struct {
	Connected    bool      `json:"connected"`
	IP           string    `json:"ip"`
	Model        string    `json:"model"`
	Nickname     string    `json:"nickname"`
	Callsign     string    `json:"callsign"`
	Stations     []Station `json:"stations"`
	BoundStation string    `json:"bound_station"`
	BoundHandle  uint32    `json:"bound_handle"`
	MyHandle     uint32    `json:"my_handle"`
	Slices       []Slice   `json:"slices"`
	TX           TxState   `json:"tx"`
	Interlock    Interlock `json:"interlock"`
}

// TxEnabled reports whether the radio would allow transmit right now
// (interlock tx_allowed and not inhibited).
func (s *Snapshot) TxEnabled() bool {
	return s.Connected && s.Interlock.TxAllowed && !s.TX.Inhibit
}

// OurStationTransmitting reports whether the bound station's transmitter is
// keyed. When the radio does not say which client is keyed, it assumes ours.
func (s *Snapshot) OurStationTransmitting() bool {
	if !s.Interlock.Transmitting() {
		return false
	}
	h := s.Interlock.TxHandle
	return h == 0 || h == s.BoundHandle || h == s.MyHandle
}

// SliceByLetter finds an in-use slice owned by the bound station.
func (s *Snapshot) SliceByLetter(letter string) (Slice, bool) {
	if s.BoundHandle == 0 {
		return Slice{}, false
	}
	for _, sl := range s.Slices {
		if sl.InUse && sl.ClientHandle == s.BoundHandle && strings.EqualFold(sl.Letter, letter) {
			return sl, true
		}
	}
	return Slice{}, false
}

// ReplyFunc receives a command's reply code (0 = ok) and message.
type ReplyFunc func(code uint32, msg string)

// replyTimeout is how long a command waits for its reply before its callback
// is called with codeNoReply.
const replyTimeout = 10 * time.Second

// Codes passed to a ReplyFunc when the radio never answered.
const (
	codeNotConnected = 0xFFFFFFFF
	codeNoReply      = 0xFFFFFFFE
)

type pendingReply struct {
	fn   ReplyFunc
	sent time.Time
}

// APIPort is the radio's TCP API port (overridden only by tests).
var APIPort = "4992"

// Radio is a non-GUI client of the SmartSDR TCP API (port 4992).
type Radio struct {
	log      *logx.Source
	onUpdate func()
	notifyCh chan struct{}

	// OnSpot (optional, set before Connect) receives "triggered" and
	// "removed" spot events. It is called on the connection reader and must
	// not block.
	OnSpot func(index int, event string)

	mu          sync.Mutex
	conn        net.Conn
	connected   bool
	ip, model   string
	nickname    string
	callsign    string
	seq         uint32
	pending     map[uint32]pendingReply
	myHandle    uint32
	slices      map[int]*Slice
	stations    map[uint32]*Station
	tx          TxState
	il          Interlock
	wantStation string
	boundHandle uint32
	boundName   string
	lastRx      time.Time
	stop        chan struct{}

	udp      *net.UDPConn
	udpPort  int
	vitaAddr *net.UDPAddr // the radio's VITA-49 port, resolved once per session
	epoch    uint64
	streams  map[uint32]*stream
}

func NewRadio(l *logx.Logger, onUpdate func()) *Radio {
	r := &Radio{log: l.Src("radio"), onUpdate: onUpdate, notifyCh: make(chan struct{}, 1)}
	go r.notifyLoop()
	return r
}

// notifyLoop delivers state-change callbacks on their own goroutine, so the
// connection reader never waits on (or deadlocks with) the bridge. Bursts of
// status lines collapse into one callback.
func (r *Radio) notifyLoop() {
	for range r.notifyCh {
		if r.onUpdate != nil {
			r.onUpdate()
		}
	}
}

// Connect starts a connection loop that reconnects until Disconnect is called.
func (r *Radio) Connect(ip, model, station string) {
	r.Disconnect()
	r.mu.Lock()
	r.ip, r.model, r.wantStation = ip, model, station
	stop := make(chan struct{})
	r.stop = stop
	r.mu.Unlock()
	go r.loop(ip, stop)
}

func (r *Radio) Disconnect() {
	r.mu.Lock()
	if r.stop != nil {
		close(r.stop)
		r.stop = nil
	}
	if r.conn != nil {
		r.conn.Close()
	}
	r.mu.Unlock()
}

// SetStation selects which GUI station's slices we control, and binds to it.
func (r *Radio) SetStation(name string) {
	r.mu.Lock()
	r.wantStation = name
	r.boundHandle, r.boundName = 0, ""
	r.mu.Unlock()
	r.tryBind()
	r.notify()
}

func (r *Radio) loop(ip string, stop chan struct{}) {
	for {
		err := r.session(ip, stop)
		select {
		case <-stop:
			r.log.Info("disconnected from radio")
			return
		default:
		}
		r.log.Warn("radio connection lost: %v; retrying in 3 s", err)
		select {
		case <-stop:
			return
		case <-time.After(3 * time.Second):
		}
	}
}

func (r *Radio) session(ip string, stop chan struct{}) error {
	r.log.Info("connecting to radio at %s", net.JoinHostPort(ip, APIPort))
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(ip, APIPort), 5*time.Second)
	if err != nil {
		return err
	}
	var radioIP net.IP
	if a, err := net.ResolveIPAddr("ip4", ip); err == nil {
		radioIP = a.IP
	}
	udp, udpPort, err := r.openUDP()
	if err != nil {
		r.log.Error("%v (audio from the radio will not work)", err)
	} else {
		go r.udpLoop(udp, radioIP)
	}
	r.mu.Lock()
	r.conn = conn
	r.udp, r.udpPort = udp, udpPort
	r.vitaAddr = &net.UDPAddr{IP: radioIP, Port: VitaPort}
	r.streams = map[uint32]*stream{}
	r.pending = map[uint32]pendingReply{}
	r.slices = map[int]*Slice{}
	r.stations = map[uint32]*Station{}
	r.tx, r.il = TxState{}, Interlock{TxAllowed: true}
	r.boundHandle, r.boundName, r.myHandle = 0, "", 0
	r.lastRx = time.Now()
	r.mu.Unlock()

	done := make(chan struct{})
	defer func() {
		close(done)
		conn.Close()
		if udp != nil {
			udp.Close()
		}
		r.mu.Lock()
		r.connected = false
		r.conn = nil
		r.udp, r.vitaAddr = nil, nil
		r.streams = map[uint32]*stream{}
		r.boundHandle, r.boundName = 0, ""
		r.mu.Unlock()
		r.notify()
	}()

	// Watchdog: ping every 2 s, drop the link if nothing heard for 10 s.
	go func() {
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-stop:
				conn.Close()
				return
			case <-t.C:
				r.mu.Lock()
				silent := time.Since(r.lastRx)
				r.mu.Unlock()
				if silent > 10*time.Second {
					r.log.Error("radio silent for %v, closing connection", silent.Round(time.Second))
					conn.Close()
					return
				}
				r.expireReplies()
				r.Command("ping", nil)
			}
		}
	}()

	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		r.mu.Lock()
		r.lastRx = time.Now()
		r.mu.Unlock()
		r.handleLine(line)
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return fmt.Errorf("connection closed by radio")
}

func (r *Radio) handleLine(line string) {
	if line == "" {
		return
	}
	switch line[0] {
	case 'V':
		r.log.Info("radio API version %s", line[1:])
	case 'H':
		h, _ := strconv.ParseUint(line[1:], 16, 32)
		r.mu.Lock()
		r.myHandle = uint32(h)
		r.connected = true
		udpPort := r.udpPort
		r.mu.Unlock()
		atomic.AddUint64(&r.epoch, 1)
		r.log.Info("connected, our client handle 0x%08X", h)
		// Interlock status arrives with "sub tx all"; there is no interlock subscription.
		for _, s := range []string{"sub client all", "sub slice all", "sub tx all", "sub radio all", "sub spot all"} {
			r.Command(s, nil)
		}
		r.Command("info", r.onInfo)
		if udpPort != 0 {
			// Tell the radio where to send our DAX audio streams (LAN clients).
			r.Command(fmt.Sprintf("client udpport %d", udpPort), func(code uint32, _ string) {
				if code == 0 {
					r.log.Info("audio streams will arrive on UDP %d", udpPort)
				}
			})
		}
		r.notify()
	case 'R':
		r.handleReply(line[1:])
	case 'S':
		r.log.Traffic("<- %s", line)
		if _, body, ok := strings.Cut(line, "|"); ok {
			if r.handleStatus(body) {
				r.notify()
			}
		}
	case 'M':
		r.log.Debug("radio message: %s", line[1:])
	default:
		r.log.Traffic("<- %s", line)
	}
}

func (r *Radio) handleReply(s string) {
	parts := strings.SplitN(s, "|", 3)
	if len(parts) < 2 {
		return
	}
	seq, _ := strconv.ParseUint(parts[0], 10, 32)
	code, _ := strconv.ParseUint(parts[1], 16, 32)
	msg := ""
	if len(parts) == 3 {
		msg = parts[2]
	}
	r.mu.Lock()
	p, ok := r.pending[uint32(seq)]
	delete(r.pending, uint32(seq))
	r.mu.Unlock()
	if ok {
		p.fn(uint32(code), msg)
	}
}

// onInfo takes the model name from the "info" reply, so a radio reached by
// manual IP (no discovery packet) still reports the right DEVICE.
func (r *Radio) onInfo(code uint32, msg string) {
	if code != 0 {
		return
	}
	model := ParseInfo(msg)["model"]
	if model == "" {
		return
	}
	r.mu.Lock()
	changed := r.model != model
	r.model = model
	r.mu.Unlock()
	if changed {
		r.log.Info("radio model %s", model)
		r.notify()
	}
}

// expireReplies calls, with codeNoReply, the callbacks of commands the radio
// has not answered within replyTimeout, so no caller waits forever and the
// map does not grow.
func (r *Radio) expireReplies() {
	var late []ReplyFunc
	r.mu.Lock()
	for seq, p := range r.pending {
		if time.Since(p.sent) > replyTimeout {
			late = append(late, p.fn)
			delete(r.pending, seq)
		}
	}
	r.mu.Unlock()
	for _, fn := range late {
		fn(codeNoReply, "no reply from radio")
	}
}

// ParseInfo splits an info reply: key="value",key=value,...
func ParseInfo(s string) map[string]string {
	m := map[string]string{}
	for _, f := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(f), "=")
		if ok {
			m[k] = strings.Trim(v, `"`)
		}
	}
	return m
}

// Command sends an API command. cb (optional) receives the reply. Failures are
// always logged, so callers rarely need a callback.
func (r *Radio) Command(cmd string, cb ReplyFunc) {
	r.mu.Lock()
	conn := r.conn
	if conn == nil {
		r.mu.Unlock()
		if cmd != "ping" {
			r.log.Warn("not connected, dropped command: %s", cmd)
		}
		if cb != nil {
			cb(codeNotConnected, "not connected")
		}
		return
	}
	r.seq++
	seq := r.seq
	quiet := cmd == "ping"
	r.pending[seq] = pendingReply{sent: time.Now(), fn: func(code uint32, msg string) {
		if code != 0 {
			r.log.Warn("command failed (0x%08X %s): %s", code, msg, cmd)
		} else if !quiet {
			r.log.Traffic("<- R%d ok %s", seq, msg)
		}
		if cb != nil {
			cb(code, msg)
		}
	}}
	r.mu.Unlock()
	if !quiet {
		r.log.Traffic("-> C%d|%s", seq, cmd)
	}
	conn.SetWriteDeadline(time.Now().Add(3 * time.Second))
	if _, err := fmt.Fprintf(conn, "C%d|%s\n", seq, cmd); err != nil {
		r.log.Error("write to radio failed: %v", err)
		conn.Close()
	}
}

// handleStatus applies a status line. It returns true if the line is one
// whose state is tracked (the caller then notifies), changed or not.
func (r *Radio) handleStatus(body string) bool {
	fields := strings.Fields(body)
	if len(fields) == 0 {
		return false
	}
	switch fields[0] {
	case "slice":
		if len(fields) < 2 {
			return false
		}
		idx, err := strconv.Atoi(fields[1])
		if err != nil {
			return false
		}
		r.applySlice(idx, ParseKV(strings.Join(fields[2:], " ")))
		return true
	case "client":
		if len(fields) < 3 {
			return false
		}
		h, _ := strconv.ParseUint(strings.TrimPrefix(fields[1], "0x"), 16, 32)
		r.applyClient(uint32(h), fields[2], ParseKV(strings.Join(fields[3:], " ")))
		return true
	case "stream":
		r.applyStream(fields)
		return true
	case "spot":
		// spot <index> triggered [pan=0x...] | spot <index> removed | spot <index> key=value ...
		if len(fields) >= 3 && (fields[2] == "triggered" || fields[2] == "removed") && r.OnSpot != nil {
			if idx, err := strconv.Atoi(fields[1]); err == nil {
				r.OnSpot(idx, fields[2])
			}
		}
		return false
	case "radio":
		if len(fields) < 2 || !strings.Contains(fields[1], "=") {
			return false // radio filter_sharpness ... etc.
		}
		kv := ParseKV(strings.Join(fields[1:], " "))
		r.mu.Lock()
		if v, ok := kv["nickname"]; ok {
			r.nickname = unescape(v)
		}
		if v, ok := kv["callsign"]; ok {
			r.callsign = v
		}
		r.mu.Unlock()
		return true
	case "transmit":
		if len(fields) < 2 || !strings.Contains(fields[1], "=") {
			return false // transmit band ... sub-objects
		}
		kv := ParseKV(strings.Join(fields[1:], " "))
		r.mu.Lock()
		if v, ok := kv["rfpower"]; ok {
			r.tx.RFPower, _ = strconv.Atoi(v)
		}
		if v, ok := kv["tunepower"]; ok {
			r.tx.TunePower, _ = strconv.Atoi(v)
		}
		if v, ok := kv["tune"]; ok {
			r.tx.Tune = v == "1"
		}
		if v, ok := kv["dax"]; ok {
			r.tx.DAX = v == "1"
		}
		// A tune across bands ("slice tune" from a TCI client, or a band
		// button in SmartSDR/Maestro) is reported only as "transmit freq=";
		// the slice status never carries the new RF_frequency (seen on an
		// AU-520, v4.3.16). The transmit frequency is the TX slice's
		// frequency, so apply it there. Skipped with XIT on, where the two
		// may differ.
		if v, ok := kv["freq"]; ok {
			if f, err := strconv.ParseFloat(v, 64); err == nil {
				hz := int64(f*1e6 + 0.5)
				for _, sl := range r.slices {
					if sl.TX && !sl.XITOn && sl.FreqHz != hz {
						r.log.Debug("slice %d frequency %d -> %d (from transmit freq)", sl.Index, sl.FreqHz, hz)
						sl.FreqHz = hz
					}
				}
			}
		}
		prevInhibit := r.tx.Inhibit
		if v, ok := kv["inhibit"]; ok {
			r.tx.Inhibit = v == "1"
		}
		inhibit := r.tx.Inhibit
		r.mu.Unlock()
		if prevInhibit != inhibit {
			r.log.Info("transmit inhibit: %v", inhibit)
		}
		return true
	case "interlock":
		if len(fields) < 2 || !strings.Contains(fields[1], "=") {
			return false
		}
		kv := ParseKV(strings.Join(fields[1:], " "))
		r.mu.Lock()
		prev := r.il
		if v, ok := kv["state"]; ok {
			r.il.State = v
		}
		if v, ok := kv["tx_allowed"]; ok {
			r.il.TxAllowed = v == "1"
		}
		if v, ok := kv["reason"]; ok {
			r.il.Reason = v
		}
		if v, ok := kv["tx_client_handle"]; ok {
			h, _ := strconv.ParseUint(strings.TrimPrefix(v, "0x"), 16, 32)
			r.il.TxHandle = uint32(h)
		}
		cur := r.il
		r.mu.Unlock()
		if prev.State != cur.State {
			r.log.Debug("interlock %s -> %s %s", prev.State, cur.State, cur.Reason)
		}
		if prev.TxAllowed != cur.TxAllowed {
			r.log.Info("transmit allowed: %v %s", cur.TxAllowed, cur.Reason)
		}
		return true
	}
	return false
}

func (r *Radio) applySlice(idx int, kv map[string]string) {
	r.mu.Lock()
	s := r.slices[idx]
	if s == nil {
		s = &Slice{Index: idx}
		r.slices[idx] = s
	}
	for k, v := range kv {
		switch k {
		case "in_use":
			s.InUse = v == "1"
		case "RF_frequency":
			if f, err := strconv.ParseFloat(v, 64); err == nil {
				s.FreqHz = int64(f*1e6 + 0.5)
			}
		case "mode":
			s.Mode = v
		case "filter_lo":
			s.FilterLo, _ = strconv.Atoi(v)
		case "filter_hi":
			s.FilterHi, _ = strconv.Atoi(v)
		case "dax":
			s.DAX, _ = strconv.Atoi(v)
		case "tx":
			s.TX = v == "1"
		case "client_handle":
			h, _ := strconv.ParseUint(strings.TrimPrefix(v, "0x"), 16, 32)
			s.ClientHandle = uint32(h)
		case "index_letter":
			s.Letter = v
		case "mode_list":
			s.ModeList = strings.Split(v, ",")
		case "sample_rate":
			s.SampleRate, _ = strconv.Atoi(v)
		case "rit_on":
			s.RITOn = v == "1"
		case "rit_freq":
			s.RITFreq, _ = strconv.Atoi(v)
		case "xit_on":
			s.XITOn = v == "1"
		case "xit_freq":
			s.XITFreq, _ = strconv.Atoi(v)
		}
	}
	if !s.InUse {
		delete(r.slices, idx)
	}
	r.mu.Unlock()
}

func (r *Radio) applyClient(h uint32, state string, kv map[string]string) {
	r.mu.Lock()
	if state == "disconnected" {
		if st, ok := r.stations[h]; ok {
			r.log.Info("station \"%s\" (%s) disconnected", st.Name, st.Program)
		}
		delete(r.stations, h)
		if h == r.boundHandle {
			r.log.Warn("bound station went away; waiting for it to return")
			r.boundHandle, r.boundName = 0, ""
		}
		r.mu.Unlock()
		r.tryBind()
		return
	}
	if h == r.myHandle {
		r.mu.Unlock()
		return
	}
	st := r.stations[h]
	if st == nil {
		st = &Station{Handle: h}
		r.stations[h] = st
	}
	if v, ok := kv["client_id"]; ok {
		st.ClientID = v
	}
	if v, ok := kv["program"]; ok {
		st.Program = unescape(v)
	}
	if v, ok := kv["station"]; ok {
		st.Name = unescape(v)
	}
	r.mu.Unlock()
	r.tryBind()
}

// tryBind binds to the wanted station if it is present and not yet bound.
// With no station chosen, a single GUI station is picked automatically.
func (r *Radio) tryBind() {
	r.mu.Lock()
	if !r.connected || r.boundHandle != 0 {
		r.mu.Unlock()
		return
	}
	var gui []*Station
	for _, st := range r.stations {
		if st.ClientID != "" && st.Name != "" {
			gui = append(gui, st)
		}
	}
	var pick *Station
	for _, st := range gui {
		if st.Name == r.wantStation {
			pick = st
		}
	}
	if pick == nil && r.wantStation == "" && len(gui) == 1 {
		pick = gui[0]
	}
	if pick == nil {
		r.mu.Unlock()
		return
	}
	r.boundHandle, r.boundName = pick.Handle, pick.Name
	id, name := pick.ClientID, pick.Name
	r.mu.Unlock()
	r.log.Info("binding to station \"%s\"", name)
	r.Command("client bind client_id="+id, nil)
}

func (r *Radio) notify() {
	select {
	case r.notifyCh <- struct{}{}:
	default: // an update is already pending
	}
}

func (r *Radio) Snapshot() Snapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := Snapshot{
		Connected:    r.connected,
		IP:           r.ip,
		Model:        r.model,
		Nickname:     r.nickname,
		Callsign:     r.callsign,
		BoundStation: r.boundName,
		BoundHandle:  r.boundHandle,
		MyHandle:     r.myHandle,
		TX:           r.tx,
		Interlock:    r.il,
	}
	for _, st := range r.stations {
		if st.ClientID != "" && st.Name != "" {
			s.Stations = append(s.Stations, *st)
		}
	}
	sort.Slice(s.Stations, func(i, j int) bool { return s.Stations[i].Name < s.Stations[j].Name })
	for _, sl := range r.slices {
		s.Slices = append(s.Slices, *sl)
	}
	sort.Slice(s.Slices, func(i, j int) bool { return s.Slices[i].Index < s.Slices[j].Index })
	return s
}

// TXSliceRate returns the DAX sample rate of the bound station's TX slice,
// or 24000 if none is known. Unlike Snapshot it copies nothing, so the TX
// audio loop can call it every packet.
func (r *Radio) TXSliceRate() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, sl := range r.slices {
		if sl.TX && sl.ClientHandle == r.boundHandle && sl.SampleRate > 0 {
			return sl.SampleRate
		}
	}
	return 24000
}

// ---- convenience commands ----

func (r *Radio) Tune(slice int, hz int64, cb ReplyFunc) {
	r.Command(fmt.Sprintf("slice tune %d %.6f", slice, float64(hz)/1e6), cb)
}
func (r *Radio) SetMode(slice int, mode string, cb ReplyFunc) {
	r.Command(fmt.Sprintf("slice set %d mode=%s", slice, mode), cb)
}
func (r *Radio) SetFilter(slice, lo, hi int) {
	r.Command(fmt.Sprintf("filt %d %d %d", slice, lo, hi), nil)
}
func (r *Radio) SetSlice(slice int, kv string) {
	r.Command(fmt.Sprintf("slice set %d %s", slice, kv), nil)
}
func (r *Radio) Xmit(on bool) {
	r.Command("xmit "+OnOff(on), nil)
}
func (r *Radio) TuneTX(on bool) {
	r.Command("transmit tune "+OnOff(on), nil)
}
func (r *Radio) SetTransmit(kv string) {
	r.Command("transmit set "+kv, nil)
}

// AddSpot creates a panadapter spot from "callsign=... rx_freq=..." pairs.
// The reply carries the new spot's index.
func (r *Radio) AddSpot(kv string, cb ReplyFunc) {
	r.Command("spot add "+kv, cb)
}
func (r *Radio) RemoveSpot(index int) {
	r.Command(fmt.Sprintf("spot remove %d", index), nil)
}

// OnOff renders a bool as the radio API's 1/0.
func OnOff(b bool) string {
	if b {
		return "1"
	}
	return "0"
}
