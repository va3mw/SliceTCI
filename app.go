package main

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/wailsapp/wails/v2/pkg/runtime"

	"github.com/va3mw/FlexTCI/internal/audio"
	"github.com/va3mw/FlexTCI/internal/bridge"
	"github.com/va3mw/FlexTCI/internal/config"
	"github.com/va3mw/FlexTCI/internal/logx"
)

// App is the Wails-bound facade over the bridge.
type App struct {
	ctx    context.Context
	log    *logx.Logger
	cfg    *config.Store
	bridge *bridge.Bridge

	logMu  sync.Mutex
	logBuf []logx.Entry
}

func NewApp(l *logx.Logger, cfg *config.Store) *App {
	a := &App{log: l, cfg: cfg}
	a.bridge = bridge.New(l, cfg, a.pushStatus)
	l.OnEntry(func(e logx.Entry) {
		a.logMu.Lock()
		a.logBuf = append(a.logBuf, e)
		a.logMu.Unlock()
	})
	return a
}

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	go a.logPump()
	a.bridge.Start()
}

func (a *App) shutdown(context.Context) {
	a.log.Log(logx.Info, "app", "shutting down")
	a.bridge.Shutdown()
	a.log.Close()
}

func (a *App) pushStatus() {
	if a.ctx != nil {
		runtime.EventsEmit(a.ctx, "status", a.bridge.Status())
	}
}

// logPump forwards log entries to the UI in batches.
func (a *App) logPump() {
	for range time.Tick(150 * time.Millisecond) {
		a.logMu.Lock()
		batch := a.logBuf
		a.logBuf = nil
		a.logMu.Unlock()
		if len(batch) > 0 && a.ctx != nil {
			runtime.EventsEmit(a.ctx, "log", batch)
		}
	}
}

// ---- bound methods ----

func (a *App) GetStatus() bridge.Status { return a.bridge.Status() }

func (a *App) GetLogs(since uint64) []logx.Entry { return a.log.Since(since) }

type Devices struct {
	Capture []audio.Device `json:"capture"`
	Render  []audio.Device `json:"render"`
	Error   string         `json:"error"`
}

func (a *App) GetDevices() Devices {
	var d Devices
	var errs []string
	var err error
	if d.Capture, err = audio.ListDevices(true); err != nil {
		errs = append(errs, err.Error())
	}
	if d.Render, err = audio.ListDevices(false); err != nil {
		errs = append(errs, err.Error())
	}
	d.Error = strings.Join(errs, "; ")
	return d
}

func (a *App) Connect(serial, ip string) error {
	return a.bridge.Connect(serial, strings.TrimSpace(ip))
}

func (a *App) Disconnect() { a.bridge.Disconnect() }

func (a *App) SelectStation(name string) { a.bridge.SelectStation(name) }

func (a *App) SetChannelCount(n int) error {
	if n < 1 || n > config.MaxChannels {
		return fmt.Errorf("channel count must be 1 to %d", config.MaxChannels)
	}
	c := a.cfg.Get()
	c.ChannelCount = n
	if err := validate(c); err != nil {
		return err
	}
	a.cfg.Update(func(c *config.Config) { c.ChannelCount = n })
	a.log.Log(logx.Info, "app", "channel count set to %d", n)
	a.bridge.ApplyChannels()
	return nil
}

func (a *App) UpdateChannel(i int, ch config.Channel) error {
	if i < 0 || i >= config.MaxChannels {
		return fmt.Errorf("bad channel %d", i)
	}
	ch.Slice = strings.ToUpper(strings.TrimSpace(ch.Slice))
	c := a.cfg.Get()
	c.Channels[i] = ch
	if err := validate(c); err != nil {
		return err
	}
	a.cfg.Update(func(c *config.Config) { c.Channels[i] = ch })
	a.log.Log(logx.Info, "app", "channel %d: Slice %s, port %d, DAX %d, RX device %q", i+1, ch.Slice, ch.Port, ch.DAX, ch.RXDevice)
	a.bridge.ApplyChannels()
	return nil
}

func (a *App) ResetChannel(i int) error {
	return a.UpdateChannel(i, config.DefaultChannel(i))
}

func validate(c config.Config) error {
	ports := map[int]int{}
	slices := map[string]int{}
	for i := 0; i < c.ChannelCount; i++ {
		ch := c.Channels[i]
		if ch.Port < 1024 || ch.Port > 65535 {
			return fmt.Errorf("channel %d: port must be 1024-65535", i+1)
		}
		if len(ch.Slice) != 1 || ch.Slice[0] < 'A' || ch.Slice[0] > 'H' {
			return fmt.Errorf("channel %d: slice must be A-H", i+1)
		}
		if ch.DAX < 1 || ch.DAX > 8 {
			return fmt.Errorf("channel %d: DAX channel must be 1-8", i+1)
		}
		if g := ch.RXGain(); g < -80 || g > 20 {
			return fmt.Errorf("channel %d: RX gain must be -80 to +20 dB", i+1)
		}
		if j, ok := ports[ch.Port]; ok {
			return fmt.Errorf("channels %d and %d both use port %d", j+1, i+1, ch.Port)
		}
		if j, ok := slices[ch.Slice]; ok {
			return fmt.Errorf("channels %d and %d both use Slice %s", j+1, i+1, ch.Slice)
		}
		ports[ch.Port], slices[ch.Slice] = i, i
	}
	return nil
}

func (a *App) SetTXDevice(name string) {
	a.cfg.Update(func(c *config.Config) { c.TXDevice = name })
	a.log.Log(logx.Info, "app", "DAX TX device: %s (applies on next key)", orAuto(name))
}

// SetAudioPath picks "radio" (direct VITA-49) or "windows" (DAX sound devices).
func (a *App) SetAudioPath(path string) error {
	if path != config.PathRadio && path != config.PathWindows {
		return fmt.Errorf("unknown audio path %q", path)
	}
	if a.cfg.Get().AudioPath == path {
		return nil
	}
	a.cfg.Update(func(c *config.Config) { c.AudioPath = path })
	label := map[string]string{config.PathRadio: "direct from the radio", config.PathWindows: "Windows DAX devices"}[path]
	a.log.Log(logx.Info, "app", "audio path: %s", label)
	a.bridge.RestartAudio()
	a.pushStatus()
	return nil
}

func orAuto(s string) string {
	if s == "" {
		return "auto"
	}
	return s
}

type Options struct {
	UnkeyTimeoutMs int    `json:"unkey_timeout_ms"`
	ProtocolName   string `json:"protocol_name"`
	SetSliceDAX    bool   `json:"set_slice_dax"`
	LogTraffic     bool   `json:"log_traffic"`
	TXGainDB       int    `json:"tx_gain_db"`
}

func (a *App) SetOptions(o Options) error {
	if o.UnkeyTimeoutMs < 500 || o.UnkeyTimeoutMs > 10000 {
		return fmt.Errorf("unkey timeout must be 500-10000 ms")
	}
	o.ProtocolName = strings.TrimSpace(o.ProtocolName)
	if o.ProtocolName == "" || strings.ContainsAny(o.ProtocolName, ":,;") {
		return fmt.Errorf("protocol name must be non-empty and not contain : , ;")
	}
	if o.TXGainDB < -40 || o.TXGainDB > 20 {
		return fmt.Errorf("TX gain must be -40 to +20 dB")
	}
	a.cfg.Update(func(c *config.Config) {
		c.UnkeyTimeoutMs, c.ProtocolName, c.SetSliceDAX, c.LogTraffic = o.UnkeyTimeoutMs, o.ProtocolName, o.SetSliceDAX, o.LogTraffic
		c.TXGainDB = o.TXGainDB
	})
	a.log.SetTraffic(o.LogTraffic)
	a.log.Log(logx.Info, "app", "options: unkey timeout %d ms, protocol name %q, auto DAX %v, traffic log %v, TX gain %d dB",
		o.UnkeyTimeoutMs, o.ProtocolName, o.SetSliceDAX, o.LogTraffic, o.TXGainDB)
	a.pushStatus()
	return nil
}

func (a *App) OpenLogFolder() {
	exec.Command("explorer", a.log.Dir()).Start()
}
