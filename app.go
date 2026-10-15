package main

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/wailsapp/wails/v2/pkg/runtime"

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
		runtime.EventsEmit(a.ctx, "status", a.bridge.Status(true))
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

// GetStatus is the UI's on-demand read. It leaves the level meters alone so
// it does not steal a peak from the next periodic push.
func (a *App) GetStatus() bridge.Status { return a.bridge.Status(false) }

func (a *App) GetLogs(since uint64) []logx.Entry { return a.log.Since(since) }

func (a *App) Connect(serial, ip string) error {
	return a.bridge.Connect(serial, strings.TrimSpace(ip))
}

func (a *App) Disconnect() { a.bridge.Disconnect() }

func (a *App) SelectStation(name string) { a.bridge.SelectStation(name) }

func (a *App) UpdateChannel(i int, ch config.Channel) error {
	if i < 0 || i >= config.MaxChannels {
		return fmt.Errorf("bad channel %d", i)
	}
	ch.Slice = strings.ToUpper(strings.TrimSpace(ch.Slice))
	c := a.cfg.Get()
	c.Channels[i] = ch
	if err := c.ValidateChannels(); err != nil {
		return err
	}
	if err := a.save(func(c *config.Config) { c.Channels[i] = ch }); err != nil {
		return err
	}
	a.log.Log(logx.Info, "app", "channel %d: Slice %s, port %d, DAX %d, RX gain %d dB", i+1, ch.Slice, ch.Port, ch.DAX, ch.RXGain())
	a.bridge.ApplyChannels()
	return nil
}

func (a *App) ResetChannel(i int) error {
	return a.UpdateChannel(i, config.DefaultChannel(i))
}

// AddChannel appends a channel using the first free slice letter, port and
// DAX channel.
func (a *App) AddChannel() error {
	c := a.cfg.Get()
	if c.ChannelCount >= config.MaxChannels {
		return fmt.Errorf("at most %d channels", config.MaxChannels)
	}
	used := func(f func(config.Channel) bool) bool {
		for i := 0; i < c.ChannelCount; i++ {
			if f(c.Channels[i]) {
				return true
			}
		}
		return false
	}
	var nc config.Channel
	for i := 0; i < config.MaxChannels; i++ {
		l := string(rune('A' + i))
		if !used(func(x config.Channel) bool { return x.Slice == l }) {
			nc = config.DefaultChannel(i)
			break
		}
	}
	for used(func(x config.Channel) bool { return x.Port == nc.Port }) {
		nc.Port++
	}
	for d := 1; d <= config.MaxDAX; d++ {
		if !used(func(x config.Channel) bool { return x.DAX == d }) {
			nc.DAX = d
			break
		}
	}
	n := c.ChannelCount
	if err := a.save(func(c *config.Config) { c.Channels[n] = nc; c.ChannelCount = n + 1 }); err != nil {
		return err
	}
	a.log.Log(logx.Info, "app", "added channel: Slice %s, port %d, DAX %d", nc.Slice, nc.Port, nc.DAX)
	a.bridge.ApplyChannels()
	return nil
}

// RemoveChannel deletes channel i; later channels move up one place.
func (a *App) RemoveChannel(i int) error {
	c := a.cfg.Get()
	if c.ChannelCount <= 1 {
		return fmt.Errorf("at least one channel is required")
	}
	if i < 0 || i >= c.ChannelCount {
		return fmt.Errorf("bad channel %d", i)
	}
	removed := c.Channels[i]
	err := a.save(func(c *config.Config) {
		chs := append(append([]config.Channel{}, c.Channels[:i]...), c.Channels[i+1:]...)
		c.Channels = append(chs, config.DefaultChannel(config.MaxChannels-1))
		c.ChannelCount--
	})
	if err != nil {
		return err
	}
	a.log.Log(logx.Info, "app", "removed channel: Slice %s, port %d", removed.Slice, removed.Port)
	a.bridge.ApplyChannels()
	return nil
}

// SetTXGain changes the TX audio trim live; the next TX audio block uses it.
// Called repeatedly while the slider is dragged, so it logs at debug level.
func (a *App) SetTXGain(db int) error {
	if db < config.MinTXGainDB || db > config.MaxTXGainDB {
		return fmt.Errorf("TX gain must be %d to %+d dB", config.MinTXGainDB, config.MaxTXGainDB)
	}
	if a.cfg.Get().TXGainDB == db {
		return nil
	}
	if err := a.save(func(c *config.Config) { c.TXGainDB = db }); err != nil {
		return err
	}
	a.log.Log(logx.Debug, "app", "TX gain %d dB", db)
	return nil
}

func (a *App) SetOptions(o config.Options) error {
	o.ProtocolName = strings.TrimSpace(o.ProtocolName)
	if err := o.Validate(); err != nil {
		return err
	}
	if err := a.save(func(c *config.Config) { c.Options = o }); err != nil {
		return err
	}
	if !o.Spots {
		if n := a.bridge.ClearSpots(); n > 0 {
			a.log.Log(logx.Info, "app", "spots turned off: removed %d TCI spots from the radio", n)
		}
	}
	a.log.SetTraffic(o.LogTraffic)
	a.log.Log(logx.Info, "app", "options: unkey timeout %d ms, max TX %d s, protocol name %q, auto DAX %v, digital on TX %v, spots %v, traffic log %v",
		o.UnkeyTimeoutMs, o.MaxTXSeconds, o.ProtocolName, o.SetSliceDAX, o.DigitalOnTX, o.Spots, o.LogTraffic)
	a.pushStatus()
	return nil
}

func (a *App) ConfigDir() string { return config.Dir() }

func (a *App) OpenLogFolder() {
	if err := exec.Command("explorer", a.log.Dir()).Start(); err != nil {
		a.log.Log(logx.Warn, "app", "cannot open log folder %s: %v", a.log.Dir(), err)
	}
}

// save applies f to the settings. A failed save is logged and returned so
// the UI shows it; the change itself still takes effect.
func (a *App) save(f func(c *config.Config)) error {
	err := a.cfg.Update(f)
	if err != nil {
		a.log.Log(logx.Error, "app", "%v", err)
	}
	return err
}
