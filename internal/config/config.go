// Package config holds persisted user settings.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// MaxChannels is the hard upper bound on TCI channels (one port per slice).
// The connected radio sets the real limit: 2, 4 or 8 slices by model.
const MaxChannels = 8

// MaxSlices is the number of slice letters a channel can use (A-H).
const MaxSlices = 8

// Limits shared by validation (the UI) and normalize (a hand-edited file).
const (
	MaxDAX                = 8
	MinRXGainDB           = -80
	MaxRXGainDB           = 20
	MinTXGainDB           = -40
	MaxTXGainDB           = 20
	MinUnkeyTimeoutMs     = 500
	MaxUnkeyTimeoutMs     = 10000
	DefaultUnkeyTimeoutMs = 2000
	MinMaxTXSeconds       = 5
	MaxMaxTXSeconds       = 3600
	DefaultProtocolName   = "ExpertSDR3"
)

type Channel struct {
	Slice    string `json:"slice"`                // slice letter, A and up (one per channel)
	Port     int    `json:"port"`                 // TCI WebSocket port
	DAX      int    `json:"dax"`                  // DAX channel streamed from the radio (1..MaxDAX)
	RXGainDB *int   `json:"rx_gain_db,omitempty"` // nil = default
}

// DefaultTXGainDB trims TCI transmit audio, which runs hot at 0 dB.
const DefaultTXGainDB = -6

// DefaultMaxTXSeconds is the TX watchdog default. FT8/FT4 overs are well
// under a minute; five minutes still allows long RTTY/SSB overs.
const DefaultMaxTXSeconds = 300

// DefaultRXGainDB is unity gain, the same level SmartSDR's DAX app gives at
// its default RX slider of 50 (reported by K0VM).
const DefaultRXGainDB = 0

// RXGain returns the channel's RX gain in dB.
func (c Channel) RXGain() int {
	if c.RXGainDB == nil {
		return DefaultRXGainDB
	}
	return *c.RXGainDB
}

// SameExceptGain reports whether two channels differ only in RX gain, which
// can change without restarting the listener.
func (c Channel) SameExceptGain(o Channel) bool {
	return c.Slice == o.Slice && c.Port == o.Port && c.DAX == o.DAX
}

// Options are the settings edited together in the Settings dialog. Config
// embeds them, so they stay flat in config.json and in the status JSON.
type Options struct {
	UnkeyTimeoutMs int    `json:"unkey_timeout_ms"`
	MaxTXSeconds   int    `json:"max_tx_seconds"` // TX watchdog: longest single transmission
	ProtocolName   string `json:"protocol_name"`
	SetSliceDAX    bool   `json:"set_slice_dax"`
	DigitalOnTX    bool   `json:"digital_on_tx"` // TCI-audio TX: USB/LSB -> DIGU/DIGL and TX DAX on
	Spots          bool   `json:"spots"`         // TCI spots <-> panadapter spots
	LogTraffic     bool   `json:"log_traffic"`
}

// Validate checks options from the UI.
func (o Options) Validate() error {
	if o.UnkeyTimeoutMs < MinUnkeyTimeoutMs || o.UnkeyTimeoutMs > MaxUnkeyTimeoutMs {
		return fmt.Errorf("unkey timeout must be %d-%d ms", MinUnkeyTimeoutMs, MaxUnkeyTimeoutMs)
	}
	if o.ProtocolName == "" || strings.ContainsAny(o.ProtocolName, ":,;") {
		return fmt.Errorf("protocol name must be non-empty and not contain : , ;")
	}
	if o.MaxTXSeconds < MinMaxTXSeconds || o.MaxTXSeconds > MaxMaxTXSeconds {
		return fmt.Errorf("max TX time must be %d-%d s", MinMaxTXSeconds, MaxMaxTXSeconds)
	}
	return nil
}

type Config struct {
	RadioIP      string      `json:"radio_ip"`     // last radio, or manual IP
	RadioSerial  string      `json:"radio_serial"` // last radio picked from discovery
	Station      string      `json:"station"`      // SmartSDR station name to bind to
	ChannelCount int         `json:"channel_count"`
	Channels     []Channel   `json:"channels"`
	TXGainDB     int         `json:"tx_gain_db"` // TX audio trim
	AutoConnect  bool        `json:"auto_connect"`
	Window       *WindowRect `json:"window,omitempty"`     // last window position; nil = centred
	WSJTXPath    string      `json:"wsjtx_path,omitempty"` // wsjtx.exe for the desktop shortcuts
	Options
}

// WindowRect is the main window's normal (not minimised or maximised)
// position in Windows workspace coordinates, physical pixels.
type WindowRect struct {
	Left   int32 `json:"left"`
	Top    int32 `json:"top"`
	Right  int32 `json:"right"`
	Bottom int32 `json:"bottom"`
}

func Default() *Config {
	c := &Config{
		ChannelCount: 1,
		TXGainDB:     DefaultTXGainDB,
		AutoConnect:  true,
		Options: Options{
			UnkeyTimeoutMs: DefaultUnkeyTimeoutMs,
			MaxTXSeconds:   DefaultMaxTXSeconds,
			ProtocolName:   DefaultProtocolName,
			SetSliceDAX:    true,
			DigitalOnTX:    true,
			Spots:          true,
		},
	}
	c.normalize()
	return c
}

// ValidateChannels checks the active channels: ranges, and no two sharing a
// port or slice.
func (c Config) ValidateChannels() error {
	ports := map[int]int{}
	slices := map[string]int{}
	last := byte('A' + MaxSlices - 1)
	for i := 0; i < c.ChannelCount; i++ {
		ch := c.Channels[i]
		if ch.Port < 1024 || ch.Port > 65535 {
			return fmt.Errorf("channel %d: port must be 1024-65535", i+1)
		}
		if len(ch.Slice) != 1 || ch.Slice[0] < 'A' || ch.Slice[0] > last {
			return fmt.Errorf("channel %d: slice must be A-%c", i+1, last)
		}
		if ch.DAX < 1 || ch.DAX > MaxDAX {
			return fmt.Errorf("channel %d: DAX channel must be 1-%d", i+1, MaxDAX)
		}
		if g := ch.RXGain(); g < MinRXGainDB || g > MaxRXGainDB {
			return fmt.Errorf("channel %d: RX gain must be %d to %+d dB", i+1, MinRXGainDB, MaxRXGainDB)
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

func DefaultChannel(i int) Channel {
	return Channel{Slice: string(rune('A' + i)), Port: 50001 + i, DAX: i + 1}
}

func (c *Config) normalize() {
	for len(c.Channels) < MaxChannels {
		c.Channels = append(c.Channels, DefaultChannel(len(c.Channels)))
	}
	c.Channels = c.Channels[:MaxChannels]
	if c.ChannelCount < 1 {
		c.ChannelCount = 1
	}
	if c.ChannelCount > MaxChannels {
		c.ChannelCount = MaxChannels
	}
	if c.UnkeyTimeoutMs < MinUnkeyTimeoutMs || c.UnkeyTimeoutMs > MaxUnkeyTimeoutMs {
		c.UnkeyTimeoutMs = DefaultUnkeyTimeoutMs
	}
	if c.MaxTXSeconds < MinMaxTXSeconds || c.MaxTXSeconds > MaxMaxTXSeconds {
		c.MaxTXSeconds = DefaultMaxTXSeconds
	}
	if c.ProtocolName == "" {
		c.ProtocolName = DefaultProtocolName
	}
}

type Store struct {
	mu   sync.Mutex
	path string
	cfg  *Config
}

func Dir() string {
	base, err := os.UserConfigDir()
	if err != nil {
		base = "."
	}
	return filepath.Join(base, "SliceTCI")
}

// legacyDirName is the settings folder used before the rename to SliceTCI.
const legacyDirName = "FlexTCI"

// migrateLegacyDir moves the pre-rename settings folder (FlexTCI) to dir when
// dir does not exist yet. If the folder cannot be moved (an old exe in it is
// still running), only config.json is copied.
func migrateLegacyDir(dir string) {
	if _, err := os.Stat(dir); err == nil {
		return
	}
	old := filepath.Join(filepath.Dir(dir), legacyDirName)
	if fi, err := os.Stat(old); err != nil || !fi.IsDir() {
		return
	}
	if os.Rename(old, dir) == nil {
		return
	}
	if b, err := os.ReadFile(filepath.Join(old, "config.json")); err == nil {
		if os.MkdirAll(dir, 0o755) == nil {
			_ = os.WriteFile(filepath.Join(dir, "config.json"), b, 0o644)
		}
	}
}

func Load() *Store {
	migrateLegacyDir(Dir())
	return LoadFrom(filepath.Join(Dir(), "config.json"))
}

func LoadFrom(path string) *Store {
	s := &Store{path: path, cfg: Default()}
	if b, err := os.ReadFile(s.path); err == nil {
		c := Default()
		c.Channels = nil
		if json.Unmarshal(b, c) == nil {
			c.normalize()
			s.cfg = c
		}
	}
	return s
}

// Get returns a copy of the config.
func (s *Store) Get() Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := *s.cfg
	c.Channels = append([]Channel(nil), s.cfg.Channels...)
	return c
}

// Read calls f with the live config under the lock, without copying it.
// For hot paths; f must not keep c or call back into the Store.
func (s *Store) Read(f func(c *Config)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f(s.cfg)
}

// Update applies f to the config and saves it. The change takes effect even
// if saving fails; the error says it was not saved.
func (s *Store) Update(f func(c *Config)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	f(s.cfg)
	s.cfg.normalize()
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return fmt.Errorf("settings not saved: %w", err)
	}
	b, err := json.MarshalIndent(s.cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("settings not saved: %w", err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return fmt.Errorf("settings not saved: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("settings not saved: %w", err)
	}
	return nil
}
