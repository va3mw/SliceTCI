package flex

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/va3mw/FlexTCI/internal/vita"
)

// VitaPort is the radio's UDP port for VITA-49 streams (overridden by tests).
var VitaPort = 4991

// StreamHandler receives decoded packets for one stream.
type StreamHandler func(p vita.Packet)

type stream struct {
	id      uint32
	typ     string
	handler StreamHandler
	tx      bool // dax_tx: we currently own DAX TX
	removed bool
}

// openUDP binds the local VITA socket. Like FlexLib it starts at 4991 and
// walks up if the port is taken (SmartSDR on the same PC already uses 4991).
func (r *Radio) openUDP() (*net.UDPConn, int, error) {
	var lastErr error
	for port := 4991; port <= 4991+40; port++ {
		c, err := net.ListenUDP("udp4", &net.UDPAddr{Port: port})
		if err == nil {
			c.SetReadBuffer(1 << 20)
			return c, port, nil
		}
		lastErr = err
	}
	return nil, 0, fmt.Errorf("no free UDP port for audio streams: %w", lastErr)
}

// udpLoop dispatches incoming VITA packets to stream handlers.
func (r *Radio) udpLoop(c *net.UDPConn) {
	buf := make([]byte, 16384)
	var unknown int
	for {
		n, _, err := c.ReadFromUDP(buf)
		if err != nil {
			return
		}
		p, err := vita.Parse(buf[:n])
		if err != nil {
			continue
		}
		r.mu.Lock()
		s := r.streams[p.StreamID]
		var h StreamHandler
		if s != nil && !s.removed {
			h = s.handler
		}
		r.mu.Unlock()
		if h == nil {
			unknown++
			if unknown == 1 || unknown%1000 == 0 {
				r.log.Debug("UDP packet for unknown stream 0x%08X (%d so far)", p.StreamID, unknown)
			}
			continue
		}
		// Copy the payload: buf is reused for the next packet.
		p.Payload = append([]byte(nil), p.Payload...)
		h(p)
	}
}

// Epoch changes every time a new radio session starts. Streams belong to a
// session, so owners recreate them when the epoch changes.
func (r *Radio) Epoch() uint64 { return atomic.LoadUint64(&r.epoch) }

// Connected reports whether the API session is up.
func (r *Radio) Connected() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.connected
}

// CreateStream sends a "stream create ..." command and waits for the stream id.
func (r *Radio) CreateStream(args string, handler StreamHandler) (uint32, error) {
	type res struct {
		code uint32
		msg  string
	}
	ch := make(chan res, 1)
	r.Command("stream create "+args, func(code uint32, msg string) { ch <- res{code, msg} })
	select {
	case rr := <-ch:
		if rr.code != 0 {
			return 0, fmt.Errorf("stream create %s: radio error 0x%08X %s", args, rr.code, rr.msg)
		}
		id, err := strconv.ParseUint(strings.TrimPrefix(strings.TrimSpace(rr.msg), "0x"), 16, 32)
		if err != nil {
			return 0, fmt.Errorf("stream create %s: bad stream id %q", args, rr.msg)
		}
		typ, _, _ := strings.Cut(strings.TrimPrefix(args, "type="), " ")
		r.mu.Lock()
		if old := r.streams[uint32(id)]; old != nil {
			old.handler = handler // status may have arrived before the reply
		} else {
			r.streams[uint32(id)] = &stream{id: uint32(id), typ: typ, handler: handler}
		}
		r.mu.Unlock()
		r.log.Info("created %s stream 0x%08X", typ, id)
		return uint32(id), nil
	case <-time.After(5 * time.Second):
		return 0, errors.New("stream create: no reply from radio")
	}
}

// RemoveStream asks the radio to remove a stream we created.
func (r *Radio) RemoveStream(id uint32) {
	r.mu.Lock()
	s := r.streams[id]
	delete(r.streams, id)
	connected := r.connected
	r.mu.Unlock()
	if s != nil && connected {
		r.Command(fmt.Sprintf("stream remove 0x%08X", id), nil)
		r.log.Info("removed %s stream 0x%08X", s.typ, id)
	}
}

// StreamAlive reports whether a stream we created still exists this session.
func (r *Radio) StreamAlive(id uint32) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.streams[id]
	return s != nil && !s.removed && r.connected
}

// StreamOwnsTX reports whether our dax_tx stream currently owns DAX TX.
func (r *Radio) StreamOwnsTX(id uint32) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.streams[id]
	return s != nil && s.tx
}

// SendVITA sends a packet to the radio's VITA port.
func (r *Radio) SendVITA(b []byte) error {
	r.mu.Lock()
	c, ip := r.udp, r.ip
	r.mu.Unlock()
	if c == nil {
		return errors.New("audio socket not open")
	}
	_, err := c.WriteToUDP(b, &net.UDPAddr{IP: net.ParseIP(ip), Port: VitaPort})
	return err
}

// applyStream handles "stream 0x... type=... k=v" and "stream 0x... removed".
// Only streams this client created are tracked; others are ignored.
func (r *Radio) applyStream(fields []string) {
	if len(fields) < 3 {
		return
	}
	id64, err := strconv.ParseUint(strings.TrimPrefix(fields[1], "0x"), 16, 32)
	if err != nil {
		return
	}
	id := uint32(id64)
	kv := ParseKV(strings.Join(fields[2:], " "))
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.streams[id]
	if _, gone := kv["removed"]; gone {
		if s != nil {
			s.removed = true
			r.log.Warn("radio removed our %s stream 0x%08X", s.typ, id)
		}
		return
	}
	if h, ok := kv["client_handle"]; ok {
		hv, _ := strconv.ParseUint(strings.TrimPrefix(h, "0x"), 16, 32)
		if uint32(hv) != r.myHandle {
			return // someone else's stream (e.g. SmartSDR DAX)
		}
	}
	if s == nil {
		// Status can arrive before the create reply; remember it.
		s = &stream{id: id, typ: kv["type"]}
		r.streams[id] = s
	}
	if v, ok := kv["tx"]; ok {
		was := s.tx
		s.tx = v == "1"
		if was != s.tx {
			r.log.Info("DAX TX ownership for stream 0x%08X: %v", id, s.tx)
		}
	}
	if g, ok := kv["gain"]; ok && s.typ == "dax_rx" {
		r.log.Debug("dax_rx stream 0x%08X dax_channel=%s slice=%s gain=%s", id, kv["dax_channel"], kv["slice"], g)
	}
}
