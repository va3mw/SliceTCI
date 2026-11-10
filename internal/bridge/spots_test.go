package bridge

import (
	"strings"
	"testing"
	"time"

	"github.com/va3mw/SliceTCI/internal/config"
)

func TestSpotColor(t *testing.T) {
	for in, want := range map[string]string{
		"16711680":   "#FFFF0000", // plain RGB: made opaque
		"4278255360": "#FF00FF00",
		"-16711936":  "#FF00FF00", // signed 32-bit ARGB
		"2164195328": "#80FF0000",
		"red":        "",
		"":           "",
		"4294967296": "",
	} {
		if got := spotColor(in); got != want {
			t.Errorf("spotColor(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestSpots: TCI spots become radio spots, are removed by index, and a click
// in SmartSDR comes back as CLICKED_ON_SPOT. The fake radio numbers spots
// from 7.
func TestSpots(t *testing.T) {
	const tciPort = 50954
	radio, b := newTestBridge(t, tciPort, nil)
	if !b.cfg.Get().Spots {
		t.Fatal("spots should be on by default")
	}
	startTestBridge(t, radio, b)
	if !radio.sent("sub spot all") {
		t.Fatal("never subscribed to spot status")
	}
	c := dialTCI(t, "127.0.0.1", tciPort)
	c.expect(t, "ready;", 3*time.Second)
	c2 := dialTCI(t, "127.0.0.1", tciPort)
	c2.expect(t, "ready;", 3*time.Second)
	settle := func() { time.Sleep(200 * time.Millisecond) }
	sentWith := func(part string) bool {
		radio.mu.Lock()
		defer radio.mu.Unlock()
		for _, s := range radio.seen {
			if strings.Contains(s, part) {
				return true
			}
		}
		return false
	}

	// Add: frequency in MHz, ARGB as #AARRGGBB, spaces as 0x7F, a lifetime.
	c.send("SPOT:DL1ABC,CW,14025000,16711680,CQ DX, up 2;")
	radio.expect(t, "spot add callsign=DL1ABC rx_freq=14.025000 mode=CW color=#FFFF0000 comment=CQ\x7fDX,up\x7f2 source=SliceTCI lifetime_seconds=900", time.Second)
	c.send("spot:k1xyz,ssb,7150000,-16711936,;")
	radio.expect(t, "spot add callsign=K1XYZ rx_freq=7.150000 mode=SSB color=#FF00FF00 source=SliceTCI lifetime_seconds=900", time.Second)
	settle()

	// A click in SmartSDR reaches every TCI client.
	radio.push("S11111111|spot 8 triggered pan=0x40000000")
	c.expect(t, "clicked_on_spot:K1XYZ,7150000;", time.Second)
	c2.expect(t, "clicked_on_spot:K1XYZ,7150000;", time.Second)

	// Delete by callsign becomes remove by index.
	c2.send("SPOT_DELETE:dl1abc;")
	radio.expect(t, "spot remove 7", time.Second)

	// Client text cannot start a second radio command.
	c.send("SPOT:W1AW\nxmit 1,CW,14000000,0,x=y;")
	radio.expect(t, "spot add callsign=W1AWXMIT\x7f1 rx_freq=14.000000 mode=CW color=#FF000000 comment=x-y source=SliceTCI lifetime_seconds=900", time.Second)
	settle()
	if radio.sent("xmit 1") {
		t.Fatal("a line break in a spot injected a radio command")
	}

	// The same callsign again replaces its spot.
	c.send("SPOT:K1XYZ,CW,7005000,0,;")
	radio.expect(t, "spot remove 8", time.Second)
	radio.expect(t, "spot add callsign=K1XYZ rx_freq=7.005000 mode=CW color=#FF000000 source=SliceTCI lifetime_seconds=900", time.Second)
	settle()

	// Clear removes our spots (9 and 10) and nobody else's.
	radio.push("S0|spot 50 callsign=VE3FOO rx_freq=14.200000 source=N1MM")
	c.send("SPOT_CLEAR;")
	settle()
	for _, want := range []string{"spot remove 9", "spot remove 10"} {
		if !radio.sent(want) {
			t.Errorf("SPOT_CLEAR did not send %q", want)
		}
	}
	if radio.sent("spot clear") || radio.sent("spot remove 50") {
		t.Fatal("SPOT_CLEAR touched spots SliceTCI did not add")
	}

	// A spot the radio expired is forgotten.
	c.send("SPOT:EXP1,CW,14010000,0,;")
	settle()
	radio.push("S0|spot 11 removed")
	settle()
	c.send("SPOT_DELETE:EXP1;")
	settle()
	if radio.sent("spot remove 11") {
		t.Error("removed a spot the radio had already expired")
	}

	// Delete right behind the add, before the radio has returned the index.
	c.send("SPOT:RACE,CW,14001000,0,;SPOT_DELETE:RACE;")
	radio.expect(t, "spot remove 12", time.Second)

	// Switched off: nothing goes to the radio and clicks are not reported.
	c.send("SPOT:KEEP,CW,14002000,0,;") // index 13
	settle()
	b.cfg.Update(func(c *config.Config) { c.Spots = false })
	c.send("SPOT:OFF1,CW,14003000,0,;SPOT_DELETE:KEEP;SPOT_CLEAR;")
	settle()
	if sentWith("callsign=OFF1") || radio.sent("spot remove 13") {
		t.Error("spot commands reached the radio with spots turned off")
	}
	for len(c.text) > 0 {
		<-c.text
	}
	// Clicks that must stay silent: spots off, a cleared spot, a foreign spot.
	radio.push("S0|spot 13 triggered")
	settle()
	b.cfg.Update(func(c *config.Config) { c.Spots = true })
	radio.push("S0|spot 10 triggered")
	radio.push("S0|spot 50 triggered")
	settle()
	for len(c.text) > 0 {
		if s := <-c.text; strings.HasPrefix(s, "clicked_on_spot") {
			t.Errorf("unexpected %q", s)
		}
	}
}
