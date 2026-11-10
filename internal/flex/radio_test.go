package flex

import (
	"net"
	"testing"
	"time"

	"github.com/va3mw/SliceTCI/internal/logx"
	"github.com/va3mw/SliceTCI/internal/vita"
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

// Audio packets from anything but the radio must be dropped.
func TestUDPOnlyFromRadio(t *testing.T) {
	l := logx.New(t.TempDir())
	defer l.Close()
	r := &Radio{log: l.Src("t"), streams: map[uint32]*stream{}}
	got := make(chan uint32, 10)
	r.streams[0x04000008] = &stream{id: 0x04000008, handler: func(p vita.Packet) { got <- p.StreamID }}
	sock, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer sock.Close()
	go r.udpLoop(sock, net.IPv4(127, 0, 0, 1))

	pkt := vita.BuildAudio(0x04000008, 0, vita.ClassFloat24k, make([]float32, 256))
	send := func(from net.IP) {
		c, err := net.DialUDP("udp4", &net.UDPAddr{IP: from}, sock.LocalAddr().(*net.UDPAddr))
		if err != nil {
			t.Skipf("cannot send from %v: %v", from, err)
		}
		defer c.Close()
		c.Write(pkt)
	}
	send(net.IPv4(127, 0, 0, 2)) // another host
	send(net.IPv4(127, 0, 0, 1)) // the radio
	select {
	case <-got:
	case <-time.After(time.Second):
		t.Fatal("packet from the radio was not delivered")
	}
	select {
	case <-got:
		t.Fatal("packet from another host was delivered")
	case <-time.After(200 * time.Millisecond):
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
