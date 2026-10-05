package bridge

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/va3mw/FlexTCI/internal/config"
	"github.com/va3mw/FlexTCI/internal/flex"
	"github.com/va3mw/FlexTCI/internal/logx"
	"github.com/va3mw/FlexTCI/internal/tci"
	"github.com/va3mw/FlexTCI/internal/vita"
)

// fakeRadio speaks just enough of the SmartSDR API for the bridge. Status
// formats follow a capture from a FLEX-6600M (API 1.4).
type fakeRadio struct {
	ln   net.Listener
	cmds chan string
	mu   sync.Mutex
	conn net.Conn
	seen []string

	// VITA-49: udp is the radio's own port (what the bridge sends TX to);
	// clientPort is where the bridge asked for streams ("client udpport").
	udp        *net.UDPConn
	clientPort int
	txPackets  chan vita.Packet
	rxStop     chan struct{}

	spotIdx int // last spot index handed out
}

const (
	fakeRXStream = 0x04000008
	fakeTXStream = 0x84000000
)

// sendRX streams 24 kHz float stereo DAX audio (constant 0.25) to the bridge
// at the real-time packet rate until rxStop closes.
func (f *fakeRadio) sendRX() {
	dst := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: f.clientPort}
	samples := make([]float32, vita.TXFrames*2)
	for i := range samples {
		samples[i] = 0.25
	}
	t := time.NewTicker(time.Duration(vita.TXFrames) * time.Second / 24000)
	defer t.Stop()
	var n uint8
	for {
		select {
		case <-f.rxStop:
			return
		case <-t.C:
		}
		f.udp.WriteToUDP(vita.BuildAudio(fakeRXStream, n, vita.ClassFloat24k, samples), dst)
		n++
	}
}

func (f *fakeRadio) readTX() {
	buf := make([]byte, 4096)
	for {
		n, _, err := f.udp.ReadFromUDP(buf)
		if err != nil {
			return
		}
		if p, err := vita.Parse(append([]byte(nil), buf[:n]...)); err == nil {
			select {
			case f.txPackets <- p:
			default:
			}
		}
	}
}

func (f *fakeRadio) sent(cmd string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.seen {
		if c == cmd {
			return true
		}
	}
	return false
}

// push sends an unsolicited status line, as the radio would.
func (f *fakeRadio) push(s string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.conn != nil {
		fmt.Fprint(f.conn, s+"\n")
	}
}

func newFakeRadio(t *testing.T) *fakeRadio {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	udp, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeRadio{spotIdx: 6, ln: ln, cmds: make(chan string, 1000), udp: udp, txPackets: make(chan vita.Packet, 10000), rxStop: make(chan struct{})}
	t.Cleanup(func() { close(f.rxStop); udp.Close(); ln.Close() })
	flex.VitaPort = udp.LocalAddr().(*net.UDPAddr).Port
	go f.serve()
	go f.readTX()
	return f
}

func (f *fakeRadio) port() string { return fmt.Sprint(f.ln.Addr().(*net.TCPAddr).Port) }

func (f *fakeRadio) serve() {
	conn, err := f.ln.Accept()
	if err != nil {
		return
	}
	defer conn.Close()
	f.mu.Lock()
	f.conn = conn
	f.mu.Unlock()
	send := f.push
	send("V1.4.0.0")
	send("H0000ABCD")
	slice := func(kv string) { send("S0000ABCD|slice 0 " + kv) }
	sc := bufio.NewScanner(conn)
	for sc.Scan() {
		seq, cmd, _ := strings.Cut(sc.Text(), "|")
		seq = strings.TrimPrefix(seq, "C")
		switch {
		case strings.HasPrefix(cmd, "client udpport "):
			fmt.Sscan(strings.TrimPrefix(cmd, "client udpport "), &f.clientPort)
			send("R" + seq + "|0|")
		case cmd == "stream create type=dax_rx dax_channel=1":
			send("R" + seq + fmt.Sprintf("|0|%08X", fakeRXStream))
			send(fmt.Sprintf("S0000ABCD|stream 0x%08X type=dax_rx dax_channel=1 slice=A client_handle=0x0000ABCD gain=50", fakeRXStream))
			go f.sendRX()
		case cmd == "stream create type=dax_tx":
			// The real radio sends the status before the reply (seen on a
			// 6600M); this ordering once deadlocked keying for 5 s.
			send(fmt.Sprintf("S0000ABCD|stream 0x%08X type=dax_tx client_handle=0x0000ABCD tx=1", fakeTXStream))
			send("R" + seq + fmt.Sprintf("|0|%08X", fakeTXStream))
		case strings.HasPrefix(cmd, "spot add "):
			// Status first, then the reply carrying the new index.
			f.spotIdx++
			send(fmt.Sprintf("S0000ABCD|spot %d %s", f.spotIdx, strings.TrimPrefix(cmd, "spot add ")))
			send(fmt.Sprintf("R%s|0|%d", seq, f.spotIdx))
		case strings.HasPrefix(cmd, "spot remove "):
			send("R" + seq + "|0|")
			send("S0000ABCD|spot " + strings.TrimPrefix(cmd, "spot remove ") + " removed")
		case cmd == "info":
			send("R" + seq + `|0|model="FLEX-6600M",chassis_serial="1234",name="RXb-VA3MW",callsign="VA3MW"`)
		case cmd == "sub interlock all" || cmd == "client program FlexTCI":
			send("R" + seq + "|500000A3|Invalid subscription object name")
		default:
			send("R" + seq + "|0|")
		}
		if cmd != "ping" {
			f.mu.Lock()
			f.seen = append(f.seen, cmd)
			f.mu.Unlock()
			f.cmds <- cmd
		}
		switch {
		case cmd == "sub client all":
			send("S0000ABCD|client 0x11111111 connected local_ptt=1 client_id=AAAA-BBBB program=SmartSDR-Win station=SHACK")
		case cmd == "sub slice all":
			slice("in_use=1 sample_rate=24000 RF_frequency=14.074000 client_handle=0x11111111 index_letter=A mode=DIGU filter_lo=100 filter_hi=2900 dax=0 tx=1 mode_list=LSB,USB,AM,CW,DIGL,DIGU,SAM,FM,NFM,DFM,RTTY")
		case cmd == "sub tx all":
			send("S0000ABCD|transmit freq=14.074000 rfpower=50 tunepower=10 tx_slice_mode=DIGU inhibit=0 dax=0")
			send("S0000ABCD|transmit band 9 band_name=20 rfpower=100 tunepower=10 hwalc_enabled=0 inhibit=0")
			send("S0000ABCD|interlock acc_txreq_enable=0 rca_txreq_enable=0 acc_tx_enabled=1 tx1_enabled=1 timeout=0")
			send("S0000ABCD|interlock tx_client_handle=0x00000000 state=READY reason= source= tx_allowed=1 amplifier=")
			send("S0000ABCD|interlock band 9 band_name=20 acc_txreq_enable=0 rca_txreq_enable=0 acc_tx_enabled=0")
		case cmd == "xmit 1":
			send("S0|interlock tx_client_handle=0x11111111 state=TRANSMITTING reason= source=SW tx_allowed=1 amplifier=")
		case cmd == "xmit 0":
			send("S0|interlock tx_client_handle=0x00000000 state=READY reason= source= tx_allowed=1 amplifier=")
		case strings.HasPrefix(cmd, "slice tune 0 "):
			slice("RF_frequency=" + strings.TrimPrefix(cmd, "slice tune 0 "))
		case strings.HasPrefix(cmd, "transmit set "):
			send("S0000ABCD|transmit " + strings.TrimPrefix(cmd, "transmit set "))
		case strings.HasPrefix(cmd, "slice set 0 "):
			slice(strings.TrimPrefix(cmd, "slice set 0 "))
		}
	}
}

func (f *fakeRadio) expect(t *testing.T, want string, within time.Duration) {
	t.Helper()
	deadline := time.After(within)
	for {
		select {
		case c := <-f.cmds:
			if c == want {
				return
			}
		case <-deadline:
			t.Fatalf("radio never received %q", want)
		}
	}
}

// fakeRender consumes TX audio in real time like a sound card would.
type fakeRender struct{ stop chan struct{} }

func startFakeRender(pull func([]float32)) *fakeRender {
	f := &fakeRender{stop: make(chan struct{})}
	go func() {
		t := time.NewTicker(10 * time.Millisecond)
		defer t.Stop()
		buf := make([]float32, 480*2)
		for {
			select {
			case <-f.stop:
				return
			case <-t.C:
				pull(buf)
			}
		}
	}()
	return f
}

func (f *fakeRender) Stop() { close(f.stop) }

// tciClient wraps a websocket and collects text and chrono frames.
type tciClient struct {
	conn   *websocket.Conn
	text   chan string
	chrono chan tci.Header
	rx     chan []float32
}

func dialTCI(t *testing.T, host string, port int) *tciClient {
	conn, _, err := websocket.DefaultDialer.Dial(fmt.Sprintf("ws://%s/", net.JoinHostPort(host, fmt.Sprint(port))), nil)
	if err != nil {
		t.Fatal(err)
	}
	c := &tciClient{conn: conn, text: make(chan string, 1000), chrono: make(chan tci.Header, 1000), rx: make(chan []float32, 1000)}
	go func() {
		for {
			mt, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if mt == websocket.TextMessage {
				c.text <- string(data)
			} else if h, err := tci.ReadHeader(data); err == nil && h.Type == tci.StreamTXChrono {
				c.chrono <- h
			} else if err == nil && h.Type == tci.StreamRXAudio {
				select {
				case c.rx <- tci.Decode(h, data[tci.HeaderSize:]):
				default:
				}
			}
		}
	}()
	return c
}

func (c *tciClient) send(s string) { c.conn.WriteMessage(websocket.TextMessage, []byte(s)) }

func (c *tciClient) expect(t *testing.T, want string, within time.Duration) {
	t.Helper()
	deadline := time.After(within)
	for {
		select {
		case s := <-c.text:
			if s == want {
				return
			}
		case <-deadline:
			t.Fatalf("client never received %q", want)
		}
	}
}

func TestEndToEnd(t *testing.T) {
	dir := t.TempDir()
	radio := newFakeRadio(t)
	flex.APIPort = radio.port()

	const tciPort = 50951
	cfg := config.LoadFrom(filepath.Join(dir, "config.json"))
	cfg.Update(func(c *config.Config) {
		c.Channels[0].Port = tciPort
		c.UnkeyTimeoutMs = 800
		c.AutoConnect = false
	})
	l := logx.New(dir)
	defer l.Close()
	l.SetTraffic(true)
	l.OnEntry(func(e logx.Entry) { t.Logf("%s %-5s %-9s %s", e.Time, e.Level, e.Source, e.Msg) })
	b := New(l, cfg, nil)
	b.newRender = func(pull func([]float32)) stopper { return startFakeRender(pull) }
	b.Start()
	defer b.Shutdown()
	if err := b.Connect("", "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	radio.expect(t, "client bind client_id=AAAA-BBBB", 3*time.Second)
	time.Sleep(200 * time.Millisecond)

	// Init burst reflects the slice.
	c := dialTCI(t, "127.0.0.1", tciPort)
	var got []string
	timeout := time.After(3 * time.Second)
	for done := false; !done; {
		select {
		case s := <-c.text:
			got = append(got, s)
			done = s == "ready;"
		case <-timeout:
			t.Fatalf("no ready; got %v", got)
		}
	}
	all := strings.Join(got, "")
	for _, want := range []string{
		"protocol:ExpertSDR3,2.0;", "device:FLEX-6600M;", "trx_count:2;", "vfo:0,0,14074000;",
		"vfo:1,0,14074000;", "rx_enable:1,true;", "modulation:1,digu;",
		"modulation:0,digu;", "tx_enable:0,true;", "trx:0,false;",
		"modulations_list:lsb,usb,am,cw,digl,digu,sam,nfm,dfm,rtty;",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("init burst missing %q: %v", want, got)
		}
	}
	radio.expect(t, "slice set 0 dax=1", 2*time.Second)
	for _, bad := range []string{"sub interlock all", "client program FlexTCI"} {
		if radio.sent(bad) {
			t.Errorf("sent rejected command %q", bad)
		}
	}

	// A client set up as "TCI Client RX2" (JTDX) addresses receiver 1 and
	// must be served this port's slice under that index.
	rx2 := dialTCI(t, "127.0.0.1", tciPort)
	rx2.expect(t, "ready;", 3*time.Second)
	rx2.send("RX_ENABLE:1,true;")
	rx2.expect(t, "rx_enable:1,true;", time.Second)
	rx2.send("VFO:1,0;")
	rx2.expect(t, "vfo:1,0,14074000;", time.Second)
	rx2.send("MODULATION:1;")
	rx2.expect(t, "modulation:1,digu;", time.Second)
	rx2.conn.Close()

	// A web page (Origin header) must be refused.
	hdr := http.Header{"Origin": []string{"https://evil.example"}}
	if ws, resp, err := websocket.DefaultDialer.Dial(fmt.Sprintf("ws://127.0.0.1:%d/", tciPort), hdr); err == nil {
		ws.Close()
		t.Fatal("connection from a web page origin was accepted")
	} else if resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("web page origin: want 403, got %v %v", resp, err)
	}

	// "localhost" may resolve to IPv6 loopback; that must work too.
	c6 := dialTCI(t, "::1", tciPort)
	c6.expect(t, "ready;", 3*time.Second)
	c6.conn.Close()

	// Transmit inhibit clears TX_ENABLE and blocks keying.
	radio.push("S0|transmit inhibit=1")
	c.expect(t, "tx_enable:0,false;", time.Second)
	c.send("TRX:0,true;")
	c.expect(t, "trx:0,false;", time.Second)
	radio.push("S0|transmit inhibit=0")
	c.expect(t, "tx_enable:0,true;", time.Second)
	if radio.sent("xmit 1") {
		t.Fatal("keyed while inhibited")
	}

	// WSJT-X waits for the AUDIO_START echo.
	c.send("AUDIO_START:0;")
	c.expect(t, "audio_start:0;", time.Second)
	c.send("AUDIO_STOP:0;")
	c.expect(t, "audio_stop:0;", time.Second)

	// Frequency set: immediate echo plus radio command.
	c.send("VFO:0,0,7074000;")
	c.expect(t, "vfo:0,0,7074000;", time.Second)
	radio.expect(t, "slice tune 0 7.074000", time.Second)

	// Setting the frequency it is already on must still be echoed: clients
	// wait for the echo of every set.
	time.Sleep(100 * time.Millisecond)
	c.send("VFO:0,0,7074000;")
	c.expect(t, "vfo:0,0,7074000;", time.Second)
	c.send("DDS:0,7074000;")
	c.expect(t, "dds:0,7074000;", time.Second)
	c.send("MODULATION:0,DIGU;")
	c.expect(t, "modulation:0,digu;", time.Second)

	// Mode set.
	c.send("MODULATION:0,USB;")
	c.expect(t, "modulation:0,usb;", time.Second)
	radio.expect(t, "slice set 0 mode=USB", time.Second)

	// PTT with radio audio.
	c.send("TRX:0,true;")
	radio.expect(t, "xmit 1", time.Second)
	c.expect(t, "trx:0,true;", time.Second)
	c.send("TRX:0,false;")
	radio.expect(t, "xmit 0", time.Second)

	// TCI audio keying without audio replies: safety unkey after the timeout.
	// The slice was left in USB with TX DAX off: before keying, FlexTCI
	// must switch to DIGU and turn TX DAX on (and leave both that way).
	c.send("TRX:0,true,tci;")
	radio.expect(t, "slice set 0 mode=DIGU", time.Second)
	radio.expect(t, "transmit set dax=1", time.Second)
	radio.expect(t, "xmit 1", time.Second)
	c.expect(t, "modulation:0,digu;", time.Second)
	select {
	case h := <-c.chrono:
		if h.Length != 2048 || h.SampleRate != 48000 || h.Channels != 2 {
			t.Errorf("unexpected chrono header %+v", h)
		}
	case <-time.After(time.Second):
		t.Fatal("no TX_CHRONO received")
	}
	start := time.Now()
	radio.expect(t, "xmit 0", 3*time.Second)
	if d := time.Since(start); d < 400*time.Millisecond {
		t.Errorf("safety unkey too early (%v)", d)
	}
	time.Sleep(200 * time.Millisecond)
	if radio.sent("transmit set dax=0") {
		t.Error("TX DAX was turned back off after unkey")
	}

	// TCI audio keying with audio answers stays keyed, then a dropped
	// connection unkeys.
	for len(c.chrono) > 0 {
		<-c.chrono
	}
	c.send("TRX:0,true,tci;")
	radio.expect(t, "xmit 1", time.Second)
	stop := time.After(1500 * time.Millisecond)
	answered := 0
loop:
	for {
		select {
		case h := <-c.chrono:
			h.Type = tci.StreamTXAudio
			c.conn.WriteMessage(websocket.BinaryMessage, tci.Encode(h, make([]float32, h.Length)))
			answered++
		case cmd := <-radio.cmds:
			if cmd == "xmit 0" {
				t.Fatal("unkeyed while client was sending audio")
			}
		case <-stop:
			break loop
		}
	}
	if answered < 50 { // ~47 chrono/s: 2048 samples = 1024 stereo frames at 48 kHz
		t.Errorf("only %d chrono requests in 1.5 s", answered)
	}
	c.conn.UnderlyingConn().Close() // simulate a crash
	radio.expect(t, "xmit 0", 3*time.Second)
}

// TestDirectAudio checks the no-DAX path: RX audio arrives as VITA-49 from
// the radio and reaches the TCI client; TCI TX audio leaves as VITA-49
// dax_tx packets at the slice rate.
func TestDirectAudio(t *testing.T) {
	dir := t.TempDir()
	radio := newFakeRadio(t)
	flex.APIPort = radio.port()
	const tciPort = 50952
	cfg := config.LoadFrom(filepath.Join(dir, "config.json"))
	cfg.Update(func(c *config.Config) {
		c.Channels[0].Port = tciPort
		zero := 0
		c.Channels[0].RXGainDB = &zero
		c.TXGainDB = 0 // the TX check below expects the client's level unchanged
		c.AutoConnect = false
	})
	l := logx.New(dir)
	defer l.Close()
	l.OnEntry(func(e logx.Entry) { t.Logf("%s %-5s %-9s %s", e.Time, e.Level, e.Source, e.Msg) })
	b := New(l, cfg, nil)
	b.Start()
	defer b.Shutdown()
	if err := b.Connect("", "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	radio.expect(t, "client bind client_id=AAAA-BBBB", 3*time.Second)
	if !radio.sent(fmt.Sprintf("client udpport %d", radio.clientPort)) || radio.clientPort == 0 {
		t.Fatal("bridge never sent client udpport")
	}

	c := dialTCI(t, "127.0.0.1", tciPort)
	c.expect(t, "ready;", 3*time.Second)
	c.send("AUDIO_START:0;")
	radio.expect(t, "stream create type=dax_rx dax_channel=1", 2*time.Second)

	// RX: 24 kHz 0.25 from the radio -> 48 kHz stereo float frames of 2048 samples.
	deadline := time.After(3 * time.Second)
	good := 0
	for good < 5 {
		select {
		case s := <-c.rx:
			if len(s) != 2048 {
				t.Fatalf("rx frame of %d samples", len(s))
			}
			if v := s[len(s)-1]; v > 0.24 && v < 0.26 {
				good++
			}
		case <-deadline:
			t.Fatalf("only %d good RX frames", good)
		}
	}

	// RX gain changes apply live without dropping the client.
	b.cfg.Update(func(c *config.Config) { g := -20; c.Channels[0].RXGainDB = &g })
	b.ApplyChannels()
	for len(c.rx) > 0 {
		<-c.rx
	}
	deadline = time.After(3 * time.Second)
	for good = 0; good < 3; {
		select {
		case s := <-c.rx:
			if v := s[len(s)-1]; v > 0.024 && v < 0.026 { // 0.25 at -20 dB
				good++
			}
		case <-deadline:
			t.Fatalf("RX gain change not applied (%d good frames)", good)
		}
	}

	// The dax_tx stream is prepared at connect, so keying must be fast.
	if !radio.sent("stream create type=dax_tx") {
		t.Fatal("dax_tx stream was not prepared at connect")
	}
	keyed := time.Now()
	c.send("TRX:0,true,tci;")
	radio.expect(t, "xmit 1", time.Second)
	if d := time.Since(keyed); d > 300*time.Millisecond {
		t.Errorf("keying took %v", d)
	}
	c.expect(t, "trx:0,true;", time.Second)
	for len(radio.txPackets) > 0 {
		<-radio.txPackets
	}
	stop := time.After(time.Second)
	start := time.Now()
	pkts, okVals := 0, 0
loop:
	for {
		select {
		case h := <-c.chrono:
			buf := make([]float32, h.Length)
			for i := range buf {
				buf[i] = 0.1
			}
			h.Type = tci.StreamTXAudio
			c.conn.WriteMessage(websocket.BinaryMessage, tci.Encode(h, buf))
		case p := <-radio.txPackets:
			if p.StreamID != fakeTXStream || p.PacketClass != vita.ClassFloat24k || p.OUI != vita.FlexOUI {
				t.Fatalf("bad TX packet %+v", p)
			}
			s := vita.Stereo(p)
			if len(s) != vita.TXFrames*2 {
				t.Fatalf("TX packet has %d samples", len(s))
			}
			pkts++
			if v := s[len(s)-1]; v > 0.09 && v < 0.11 {
				okVals++
			}
		case <-stop:
			break loop
		}
	}
	rate := float64(pkts) / time.Since(start).Seconds()
	if rate < 150 || rate > 220 { // 24000/128 = 187.5 packets/s
		t.Errorf("TX packet rate %.1f/s", rate)
	}
	if okVals < pkts/2 {
		t.Errorf("only %d of %d TX packets carried the client's audio", okVals, pkts)
	}
	c.send("TRX:0,false;")
	radio.expect(t, "xmit 0", time.Second)
}

// TestTXWatchdog: a client that keys and never unkeys (while staying
// connected) is cut off at the max TX time.
func TestTXWatchdog(t *testing.T) {
	dir := t.TempDir()
	radio := newFakeRadio(t)
	flex.APIPort = radio.port()
	const tciPort = 50953
	cfg := config.LoadFrom(filepath.Join(dir, "config.json"))
	cfg.Update(func(c *config.Config) {
		c.Channels[0].Port = tciPort
		c.AutoConnect = false
		c.MaxTXSeconds = 5
	})
	l := logx.New(dir)
	defer l.Close()
	l.OnEntry(func(e logx.Entry) { t.Logf("%s %-5s %-9s %s", e.Time, e.Level, e.Source, e.Msg) })
	b := New(l, cfg, nil)
	b.Start()
	defer b.Shutdown()
	if err := b.Connect("", "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	radio.expect(t, "client bind client_id=AAAA-BBBB", 3*time.Second)
	c := dialTCI(t, "127.0.0.1", tciPort)
	c.expect(t, "ready;", 3*time.Second)

	c.send("TRX:0,true;") // radio audio: no TCI audio timeout applies
	radio.expect(t, "xmit 1", time.Second)
	start := time.Now()
	radio.expect(t, "xmit 0", 8*time.Second)
	if d := time.Since(start); d < 4500*time.Millisecond {
		t.Errorf("watchdog fired after %v, before the 5 s limit", d)
	}
	c.expect(t, "trx:0,false;", time.Second)
}
