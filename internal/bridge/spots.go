package bridge

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/va3mw/SliceTCI/internal/tci"
)

// maxSpots bounds the spots tracked at once: callsigns come from the client.
const maxSpots = 2000

// spotLifetimeSeconds is how long a spot added over TCI stays on the
// panadapter. TCI has no expiry of its own, so without this spots from a
// client that never deletes them would pile up.
const spotLifetimeSeconds = 900

// spot is a panadapter spot a TCI client asked for. Spots are tracked by
// callsign so they can be removed by index later; only tracked spots are ever
// removed, so spots from other sources (cluster, N1MM) are left alone.
type spot struct {
	index   int // the radio's spot index; -1 until "spot add" is answered
	hz      int64
	epoch   uint64 // radio session the index belongs to
	expires time.Time
}

// spotToken makes client text safe as one value of a radio command: spaces
// become 0x7F as the radio expects, control characters are dropped (a line
// break would start a new command) and "=" is replaced because SmartSDR
// splits status values on it.
func spotToken(s string, max int) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(s) {
		switch {
		case b.Len() >= max:
			return b.String()
		case r == ' ':
			b.WriteByte(0x7f)
		case r == '=':
			b.WriteByte('-')
		case r > ' ' && r != 0x7f:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// spotColor converts a TCI ARGB number to the radio's #AARRGGBB. Clients send
// it unsigned or as a signed 32-bit value; a zero alpha (plain RGB) is made
// opaque so the spot is visible. Returns "" if s is not a colour.
func spotColor(s string) string {
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil || v < -1<<31 || v > 1<<32-1 {
		return ""
	}
	argb := uint32(v)
	if argb>>24 == 0 {
		argb |= 0xFF000000
	}
	return fmt.Sprintf("#%08X", argb)
}

// addSpot handles SPOT:callsign,mode,freqHz,ARGB,text;
func (b *Bridge) addSpot(c *client, a []string) {
	if len(a) < 3 {
		return
	}
	call := spotToken(strings.ToUpper(a[0]), 32)
	hz, err := strconv.ParseInt(a[2], 10, 64)
	if call == "" || err != nil || hz <= 0 {
		c.ch.lg.Warn("#%d bad spot %q", c.id, strings.Join(a, ","))
		return
	}
	cmd := fmt.Sprintf("callsign=%s rx_freq=%.6f", call, float64(hz)/1e6)
	if m := spotToken(strings.ToUpper(a[1]), 16); m != "" {
		cmd += " mode=" + m
	}
	if len(a) >= 4 {
		if col := spotColor(a[3]); col != "" {
			cmd += " color=" + col
		}
	}
	if len(a) >= 5 { // the text may itself contain commas
		if text := spotToken(strings.Join(a[4:], ","), 100); text != "" {
			cmd += " comment=" + text
		}
	}
	cmd += fmt.Sprintf(" source=SliceTCI lifetime_seconds=%d", spotLifetimeSeconds)

	now := time.Now()
	epoch := b.radio.Epoch()
	// The radio expires the spot by itself; keep ours a little longer so a
	// late click is still recognised.
	sp := &spot{index: -1, hz: hz, epoch: epoch, expires: now.Add((spotLifetimeSeconds + 60) * time.Second)}
	b.spotMu.Lock()
	for k, s := range b.spots {
		if s.epoch != epoch || now.After(s.expires) {
			delete(b.spots, k)
		}
	}
	old := b.spots[call]
	if old == nil && len(b.spots) >= maxSpots {
		b.spotMu.Unlock()
		c.ch.lg.Warn("#%d spot %s dropped: %d spots are already shown", c.id, call, maxSpots)
		return
	}
	if b.spots == nil {
		b.spots = map[string]*spot{}
	}
	b.spots[call] = sp
	b.spotMu.Unlock()

	// A callsign has one spot: a new one replaces the old. An old spot still
	// waiting for its index is removed when its reply arrives.
	if old != nil && old.index >= 0 {
		b.radio.RemoveSpot(old.index)
	}
	c.ch.lg.Debug("#%d spot %s on %d Hz", c.id, call, hz)
	b.radio.AddSpot(cmd, func(code uint32, msg string) {
		idx, err := strconv.Atoi(strings.TrimSpace(msg))
		ok := code == 0 && err == nil
		b.spotMu.Lock()
		current := b.spots[call] == sp
		if current {
			if ok {
				sp.index = idx
			} else {
				delete(b.spots, call)
			}
		}
		b.spotMu.Unlock()
		switch {
		case ok && !current: // deleted or replaced while the radio was answering
			b.radio.RemoveSpot(idx)
		case code == 0 && err != nil:
			b.lg.Warn("spot %s: radio did not return a spot index (%q); it cannot be removed over TCI", call, msg)
		}
	})
}

// deleteSpot handles SPOT_DELETE:callsign;
func (b *Bridge) deleteSpot(c *client, call string) {
	call = spotToken(strings.ToUpper(call), 32)
	b.spotMu.Lock()
	sp := b.spots[call]
	delete(b.spots, call)
	b.spotMu.Unlock()
	if sp == nil {
		c.ch.lg.Debug("#%d SPOT_DELETE %s: no such TCI spot", c.id, call)
		return
	}
	if sp.index >= 0 && sp.epoch == b.radio.Epoch() {
		b.radio.RemoveSpot(sp.index)
	}
}

// ClearSpots removes every spot added over TCI, one by one. It never sends
// "spot clear", which would also wipe spots from other programs.
func (b *Bridge) ClearSpots() int {
	b.spotMu.Lock()
	spots := b.spots
	b.spots = nil
	b.spotMu.Unlock()
	epoch := b.radio.Epoch()
	for _, sp := range spots {
		if sp.index >= 0 && sp.epoch == epoch {
			b.radio.RemoveSpot(sp.index)
		}
	}
	return len(spots)
}

// onSpotEvent handles "spot <index> triggered" (clicked in SmartSDR) and
// "spot <index> removed" from the radio. It runs on the radio's reader
// goroutine and must not block.
func (b *Bridge) onSpotEvent(index int, event string) {
	epoch := b.radio.Epoch()
	var call string
	var hz int64
	b.spotMu.Lock()
	for k, sp := range b.spots {
		if sp.index == index && sp.epoch == epoch {
			call, hz = k, sp.hz
			if event == "removed" { // expired, or removed in SmartSDR
				delete(b.spots, k)
			}
			break
		}
	}
	b.spotMu.Unlock()
	if call == "" || event != "triggered" || !b.cfg.Get().Spots {
		return
	}
	b.lg.Info("spot %s clicked in SmartSDR (%d Hz)", call, hz)
	line := tci.Fmt("clicked_on_spot", call, hz)
	for _, ch := range b.channelList() {
		ch.broadcast(line)
	}
}
