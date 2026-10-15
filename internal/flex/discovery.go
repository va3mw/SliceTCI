package flex

import (
	"context"
	"encoding/binary"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/va3mw/FlexTCI/internal/logx"
)

// RadioInfo is one radio seen in the UDP discovery broadcast.
type RadioInfo struct {
	Serial   string `json:"serial"`
	Model    string `json:"model"`
	Nickname string `json:"nickname"`
	Callsign string `json:"callsign"`
	IP       string `json:"ip"`
	Version  string `json:"version"`
	Status   string `json:"status"`
	Stations string `json:"stations"`
	lastSeen time.Time
}

// Discovery listens for FlexRadio VITA-49 discovery packets on UDP 4992.
type Discovery struct {
	log      *logx.Source
	mu       sync.Mutex
	radios   map[string]*RadioInfo
	onChange func()
	conn     *net.UDPConn
	quit     chan struct{}
}

func NewDiscovery(l *logx.Logger, onChange func()) *Discovery {
	return &Discovery{log: l.Src("discovery"), radios: map[string]*RadioInfo{}, onChange: onChange, quit: make(chan struct{})}
}

func (d *Discovery) Start() {
	go d.run()
	go d.expire()
}

// Stop closes the socket and ends the listen-retry and expiry goroutines.
func (d *Discovery) Stop() {
	d.mu.Lock()
	select {
	case <-d.quit:
	default:
		close(d.quit)
	}
	if d.conn != nil {
		d.conn.Close()
	}
	d.mu.Unlock()
}

func (d *Discovery) run() {
	// SmartSDR also listens on UDP 4992, so the port must be opened shared.
	lc := net.ListenConfig{Control: reuseAddr}
	warned := false
	for {
		pc, err := lc.ListenPacket(context.Background(), "udp4", ":4992")
		if err != nil {
			if !warned {
				d.log.Warn("cannot listen for radio discovery on UDP 4992: %v (manual IP still works; retrying quietly)", err)
				warned = true
			} else {
				d.log.Debug("discovery listen retry failed: %v", err)
			}
			select {
			case <-d.quit:
				return
			case <-time.After(10 * time.Second):
			}
			continue
		}
		conn := pc.(*net.UDPConn)
		d.mu.Lock()
		select {
		case <-d.quit: // stopped while we were opening
			d.mu.Unlock()
			conn.Close()
			return
		default:
		}
		d.conn = conn
		d.mu.Unlock()
		d.log.Info("listening for radio discovery on UDP 4992 (shared)")
		buf := make([]byte, 2048)
		for {
			n, _, err := conn.ReadFromUDP(buf)
			if err != nil {
				d.log.Debug("discovery socket closed: %v", err)
				return
			}
			if info := parseDiscovery(buf[:n]); info != nil {
				d.update(info)
			}
		}
	}
}

func (d *Discovery) update(info *RadioInfo) {
	info.lastSeen = time.Now()
	d.mu.Lock()
	old, existed := d.radios[info.Serial]
	changed := !existed || old.IP != info.IP || old.Status != info.Status || old.Stations != info.Stations
	d.radios[info.Serial] = info
	d.mu.Unlock()
	if !existed {
		d.log.Info("found %s \"%s\" serial %s at %s (v%s)", info.Model, info.Nickname, info.Serial, info.IP, info.Version)
	}
	if changed && d.onChange != nil {
		d.onChange()
	}
}

func (d *Discovery) expire() {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-d.quit:
			return
		case <-t.C:
		}
		removed := false
		d.mu.Lock()
		for k, r := range d.radios {
			if time.Since(r.lastSeen) > 15*time.Second {
				delete(d.radios, k)
				removed = true
			}
		}
		d.mu.Unlock()
		if removed && d.onChange != nil {
			d.onChange()
		}
	}
}

func (d *Discovery) List() []RadioInfo {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]RadioInfo, 0, len(d.radios))
	for _, r := range d.radios {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Serial < out[j].Serial })
	return out
}

func (d *Discovery) Find(serial string) (RadioInfo, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	r, ok := d.radios[serial]
	if !ok {
		return RadioInfo{}, false
	}
	return *r, true
}

// parseDiscovery decodes the VITA-49 header and the key=value payload.
func parseDiscovery(b []byte) *RadioInfo {
	if len(b) < 8 {
		return nil
	}
	hdr := binary.BigEndian.Uint32(b[0:4])
	off := 4
	pktType := hdr >> 28
	if pktType == 1 || pktType == 3 || pktType == 4 { // has stream id
		off += 4
	}
	if hdr&(1<<27) != 0 { // class id
		off += 8
	}
	if (hdr>>22)&3 != 0 { // integer timestamp
		off += 4
	}
	if (hdr>>20)&3 != 0 { // fractional timestamp
		off += 8
	}
	words := int(hdr & 0xFFFF)
	end := words * 4
	if end > len(b) || end <= off {
		end = len(b)
	}
	if off >= end {
		return nil
	}
	payload := strings.TrimRight(string(b[off:end]), "\x00 ")
	kv := ParseKV(payload)
	if kv["serial"] == "" || kv["ip"] == "" {
		return nil
	}
	return &RadioInfo{
		Serial:   kv["serial"],
		Model:    kv["model"],
		Nickname: unescape(kv["nickname"]),
		Callsign: kv["callsign"],
		IP:       kv["ip"],
		Version:  kv["version"],
		Status:   kv["status"],
		Stations: unescape(kv["gui_client_stations"]),
	}
}

// ParseKV splits "a=1 b=2 c" into a map. Bare words map to "".
func ParseKV(s string) map[string]string {
	m := map[string]string{}
	for _, f := range strings.Fields(s) {
		k, v, _ := strings.Cut(f, "=")
		m[k] = v
	}
	return m
}

// unescape reverses the radio's encoding of spaces (0x7F) in names.
func unescape(s string) string { return strings.ReplaceAll(s, "\x7f", " ") }
