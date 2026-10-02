package bridge

import (
	"bufio"
	"fmt"
	"net"
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
)

// fakeRadio speaks just enough of the SmartSDR API for the bridge. Status
// formats follow a capture from a FLEX-6600M (API 1.4).
type fakeRadio struct {
	ln   net.Listener
	cmds chan string
	mu   sync.Mutex
	conn net.Conn
	seen []string
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
	f := &fakeRadio{ln: ln, cmds: make(chan string, 1000)}
	go f.serve()
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
		switch cmd {
		case "info":
			send("R" + seq + `|0|model="FLEX-6600M",chassis_serial="1234",name="RXb-VA3MW",callsign="VA3MW"`)
		case "sub interlock all", "client program FlexTCI":
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
}

func dialTCI(t *testing.T, host string, port int) *tciClient {
	conn, _, err := websocket.DefaultDialer.Dial(fmt.Sprintf("ws://%s/", net.JoinHostPort(host, fmt.Sprint(port))), nil)
	if err != nil {
		t.Fatal(err)
	}
	c := &tciClient{conn: conn, text: make(chan string, 1000), chrono: make(chan tci.Header, 1000)}
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
		c.TXDevice = "__no_such_device__" // never touch real audio hardware in tests
		c.AutoConnect = false
	})
	l := logx.New(dir)
	defer l.Close()
	l.SetTraffic(true)
	l.OnEntry(func(e logx.Entry) { t.Logf("%s %-5s %-9s %s", e.Time, e.Level, e.Source, e.Msg) })
	b := New(l, cfg, nil)
	b.newRender = func(_ string, pull func([]float32)) stopper { return startFakeRender(pull) }
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
		"protocol:ExpertSDR3,2.0;", "device:FLEX-6600M;", "trx_count:1;", "vfo:0,0,14074000;",
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
	c.send("TRX:0,true,tci;")
	radio.expect(t, "transmit set dax=1", time.Second) // USB mode, so DAX source is switched
	radio.expect(t, "xmit 1", time.Second)
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
	radio.expect(t, "transmit set dax=0", time.Second)

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
