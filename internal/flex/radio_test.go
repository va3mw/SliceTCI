package flex

import (
	"net"
	"testing"
)

func TestParseInfo(t *testing.T) {
	m := ParseInfo(`model="FLEX-6600M",chassis_serial="1234",name="RXb-VA3MW",num_scu=2`)
	if m["model"] != "FLEX-6600M" || m["name"] != "RXb-VA3MW" || m["num_scu"] != "2" {
		t.Errorf("%v", m)
	}
}

func TestOurStationTransmitting(t *testing.T) {
	s := Snapshot{BoundHandle: 0x11, MyHandle: 0x22, Interlock: Interlock{State: "TRANSMITTING"}}
	for h, want := range map[uint32]bool{0: true, 0x11: true, 0x22: true, 0x33: false} {
		s.Interlock.TxHandle = h
		if got := s.OurStationTransmitting(); got != want {
			t.Errorf("tx handle 0x%X: got %v", h, got)
		}
	}
	s.Interlock.State = "READY"
	s.Interlock.TxHandle = 0x11
	if s.OurStationTransmitting() {
		t.Error("READY counted as transmitting")
	}
}

func TestTxEnabled(t *testing.T) {
	s := Snapshot{Connected: true, Interlock: Interlock{TxAllowed: true}}
	if !s.TxEnabled() {
		t.Error("should be enabled")
	}
	s.TX.Inhibit = true
	if s.TxEnabled() {
		t.Error("inhibit ignored")
	}
}

// Discovery must be able to share UDP 4992 with SmartSDR.
func TestReuseAddrSharesPort(t *testing.T) {
	lc := net.ListenConfig{Control: reuseAddr}
	a, err := lc.ListenPacket(t.Context(), "udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := lc.ListenPacket(t.Context(), "udp4", a.LocalAddr().String())
	if err != nil {
		t.Fatalf("second shared bind failed: %v", err)
	}
	b.Close()
}
