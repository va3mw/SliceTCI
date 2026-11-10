package bridge

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/va3mw/SliceTCI/internal/logx"
)

const fullSliceA = "S0000ABCD|slice 0 in_use=1 sample_rate=24000 RF_frequency=14.074000 client_handle=0x11111111 index_letter=A mode=DIGU filter_lo=100 filter_hi=2900 dax=1 tx=1 mode_list=LSB,USB,AM,CW,DIGL,DIGU,SAM,FM,NFM,DFM,RTTY"

// TestSliceFlapHoldsFrequency reproduces WSJT-X's "TCI audio cannot be
// started as frequency is OOB": the slice drops out for a moment while the
// radio reshuffles TX assignment, and 0.6.0 pushed vfo 0 to the client.
func TestSliceFlapHoldsFrequency(t *testing.T) {
	const tciPort = 50956
	radio, b := newTestBridge(t, tciPort, nil)
	var mu sync.Mutex
	var warns []string
	b.log.OnEntry(func(e logx.Entry) {
		if e.Level == logx.Warn.String() && strings.Contains(e.Msg, "not open") {
			mu.Lock()
			warns = append(warns, e.Msg)
			mu.Unlock()
		}
	})
	startTestBridge(t, radio, b)
	time.Sleep(200 * time.Millisecond)

	c := dialTCI(t, "127.0.0.1", tciPort)
	c.expect(t, "ready;", 3*time.Second)

	// Slice A vanishes and returns 150 ms later.
	radio.push("S0000ABCD|slice 0 in_use=0")
	c.expect(t, "tx_enable:0,false;", time.Second)
	// A set arriving during the gap must wait for the slice, not be dropped.
	c.send("VFO:0,0,14076000;")
	time.Sleep(150 * time.Millisecond)
	radio.push(fullSliceA)
	radio.expect(t, "slice tune 0 14.076000", 3*time.Second)
	c.expect(t, "tx_enable:0,true;", time.Second)
	c.expect(t, "vfo:0,0,14076000;", time.Second)

	// A second flap: nothing may report a zero or changed frequency.
	radio.push("S0000ABCD|slice 0 in_use=0")
	c.send("VFO:0,0;")
	c.expect(t, "vfo:0,0,14076000;", time.Second)
	radio.push(strings.Replace(fullSliceA, "14.074000", "14.076000", 1))
	c.expectNone(t, "vfo:0,0,0;", 500*time.Millisecond)

	// Short flaps are not logged as a closed slice.
	time.Sleep(absentGrace + 300*time.Millisecond)
	mu.Lock()
	n := len(warns)
	mu.Unlock()
	if n != 0 {
		t.Fatalf("transient flap logged as closed: %v", warns)
	}

	// A real close is still reported.
	radio.push("S0000ABCD|slice 0 in_use=0")
	time.Sleep(absentGrace + 300*time.Millisecond)
	mu.Lock()
	n = len(warns)
	mu.Unlock()
	if n != 1 {
		t.Fatalf("closed slice: want 1 warning, got %d", n)
	}
}

// TestInitWaitsForSlice: a client connecting while the slice is not yet open
// gets READY only once it is, with the real frequency in the init burst.
func TestInitWaitsForSlice(t *testing.T) {
	const tciPort = 50957
	radio, b := newTestBridge(t, tciPort, nil)
	startTestBridge(t, radio, b)
	time.Sleep(200 * time.Millisecond)
	radio.push("S0000ABCD|slice 0 in_use=0")
	time.Sleep(100 * time.Millisecond)

	c := dialTCI(t, "127.0.0.1", tciPort)
	time.Sleep(300 * time.Millisecond)
	radio.push(strings.Replace(fullSliceA, "14.074000", "7.074000", 1))
	deadline := time.After(3 * time.Second)
	sawFreq := false
	for {
		select {
		case s := <-c.text:
			switch s {
			case "vfo:0,0,0;":
				t.Fatal("init reported frequency 0")
			case "vfo:0,0,7074000;":
				sawFreq = true
			case "ready;":
				if !sawFreq {
					t.Fatal("ready before the real frequency")
				}
				return
			}
		case <-deadline:
			t.Fatal("no ready;")
		}
	}
}
