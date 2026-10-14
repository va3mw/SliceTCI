package bridge

import (
	"time"

	"github.com/va3mw/FlexTCI/internal/flex"
	"github.com/va3mw/FlexTCI/internal/tci"
)

// view is what a TCI client sees for transceiver 0 of a channel. Each
// channel keeps the last view it sent and pushes only the differences.
type view struct {
	Present   bool
	SliceIdx  int
	Freq      int64
	Mode      string // TCI name
	FLo, FHi  int
	TRX       bool
	Tune      bool
	Drive     int
	TuneDrive int
	TXEnable  bool
	RITOn     bool
	RIT       int
	XITOn     bool
	XIT       int
}

func computeView(s *flex.Snapshot, letter string) view {
	v := view{SliceIdx: -1}
	sl, ok := s.SliceByLetter(letter)
	if !s.Connected || !ok {
		return v
	}
	v.Present = true
	v.SliceIdx = sl.Index
	v.Freq = sl.FreqHz
	v.Mode = tci.FlexToTCI(sl.Mode)
	v.FLo, v.FHi = sl.FilterLo, sl.FilterHi
	v.RITOn, v.RIT, v.XITOn, v.XIT = sl.RITOn, sl.RITFreq, sl.XITOn, sl.XITFreq
	v.Drive, v.TuneDrive = s.TX.RFPower, s.TX.TunePower
	v.TXEnable = s.TxEnabled()
	v.Tune = sl.TX && s.TX.Tune
	v.TRX = sl.TX && s.OurStationTransmitting() && !s.TX.Tune
	return v
}

// pin holds an optimistic value we already echoed to clients. Until the radio
// reports the same value (or the pin expires) the radio's older value is not
// pushed, which avoids the stale-echo oscillation seen in other servers.
// base is the radio's value when the pin was set: a report that differs from
// both means the radio moved somewhere else, and the client must be told
// once the pin expires.
type pin struct {
	val   any
	base  any
	until time.Time
}

const pinHold = 1500 * time.Millisecond

// pinField returns the view field a pin key stands for.
func pinField(v *view, key string) any {
	switch key {
	case "freq":
		return v.Freq
	case "mode":
		return v.Mode
	case "trx":
		return v.TRX
	case "tune":
		return v.Tune
	case "drive":
		return v.Drive
	case "tune_drive":
		return v.TuneDrive
	}
	return nil
}

// applyPins overrides v with live pins. It returns true when a pin is hiding
// a radio value that is neither the pinned value nor the value from before
// the set.
func applyPins(v *view, pins map[string]pin) (moved bool) {
	now := time.Now()
	check := func(key string, cur any, set func(any)) {
		p, ok := pins[key]
		if !ok {
			return
		}
		if cur == p.val || now.After(p.until) {
			delete(pins, key)
			return
		}
		if cur != p.base {
			moved = true
		}
		set(p.val)
	}
	check("freq", v.Freq, func(x any) { v.Freq = x.(int64) })
	check("mode", v.Mode, func(x any) { v.Mode = x.(string) })
	check("trx", v.TRX, func(x any) { v.TRX = x.(bool) })
	check("tune", v.Tune, func(x any) { v.Tune = x.(bool) })
	check("drive", v.Drive, func(x any) { v.Drive = x.(int) })
	check("tune_drive", v.TuneDrive, func(x any) { v.TuneDrive = x.(int) })
	return moved
}

func freqLines(f int64) []string {
	return []string{tci.Fmt("vfo", 0, 0, f), tci.Fmt("vfo", 0, 1, f), tci.Fmt("dds", 0, f)}
}

// diff returns the TCI lines needed to move a client from old to new.
// full=true sends everything.
func diff(old, cur view, full bool) []string {
	var out []string
	if full || old.Freq != cur.Freq {
		out = append(out, freqLines(cur.Freq)...)
	}
	if full {
		out = append(out, tci.Fmt("if", 0, 0, 0), tci.Fmt("if", 0, 1, 0))
	}
	if (full || old.Mode != cur.Mode) && cur.Mode != "" {
		out = append(out, tci.Fmt("modulation", 0, cur.Mode))
	}
	if full || old.FLo != cur.FLo || old.FHi != cur.FHi {
		out = append(out, tci.Fmt("rx_filter_band", 0, cur.FLo, cur.FHi))
	}
	if full || old.TXEnable != cur.TXEnable {
		out = append(out, tci.Fmt("tx_enable", 0, cur.TXEnable))
	}
	if full || old.TRX != cur.TRX {
		out = append(out, tci.Fmt("trx", 0, cur.TRX))
	}
	if full || old.Tune != cur.Tune {
		out = append(out, tci.Fmt("tune", 0, cur.Tune))
	}
	if full || old.Drive != cur.Drive {
		out = append(out, tci.Fmt("drive", 0, cur.Drive))
	}
	if full || old.TuneDrive != cur.TuneDrive {
		out = append(out, tci.Fmt("tune_drive", 0, cur.TuneDrive))
	}
	if full {
		out = append(out, tci.Fmt("split_enable", 0, false))
	}
	if full || old.RITOn != cur.RITOn {
		out = append(out, tci.Fmt("rit_enable", 0, cur.RITOn))
	}
	if full || old.RIT != cur.RIT {
		out = append(out, tci.Fmt("rit_offset", 0, cur.RIT))
	}
	if full || old.XITOn != cur.XITOn {
		out = append(out, tci.Fmt("xit_enable", 0, cur.XITOn))
	}
	if full || old.XIT != cur.XIT {
		out = append(out, tci.Fmt("xit_offset", 0, cur.XIT))
	}
	return out
}
