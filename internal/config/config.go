// Package config holds persisted user settings.
package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// MaxChannels is the hard upper bound on TCI channels (one port per slice).
const MaxChannels = 8

type Channel struct {
	Slice    string `json:"slice"`                // slice letter A..H
	Port     int    `json:"port"`                 // TCI WebSocket port
	DAX      int    `json:"dax"`                  // DAX channel streamed from the radio (1..8)
	RXGainDB *int   `json:"rx_gain_db,omitempty"` // nil = default
}

// DefaultRXGainDB brings the radio's full-scale DAX stream down to the level
// digital-mode apps expect (WSJT-X wants roughly 30 dB on its meter; the raw
// stream reads about 76 dB).
const DefaultRXGainDB = -40

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

type Config struct {
	RadioIP        string    `json:"radio_ip"`     // last radio, or manual IP
	RadioSerial    string    `json:"radio_serial"` // last radio picked from discovery
	Station        string    `json:"station"`      // SmartSDR station name to bind to
	ChannelCount   int       `json:"channel_count"`
	Channels       []Channel `json:"channels"`
	TXGainDB       int       `json:"tx_gain_db"` // TX audio trim
	UnkeyTimeoutMs int       `json:"unkey_timeout_ms"`
	ProtocolName   string    `json:"protocol_name"`
	SetSliceDAX    bool      `json:"set_slice_dax"`
	LogTraffic     bool      `json:"log_traffic"`
	AutoConnect    bool      `json:"auto_connect"`
}

func Default() *Config {
	c := &Config{
		ChannelCount:   1,
		UnkeyTimeoutMs: 2000,
		ProtocolName:   "ExpertSDR3",
		SetSliceDAX:    true,
		AutoConnect:    true,
	}
	c.normalize()
	return c
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
	if c.UnkeyTimeoutMs < 500 {
		c.UnkeyTimeoutMs = 2000
	}
	if c.ProtocolName == "" {
		c.ProtocolName = "ExpertSDR3"
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
	return filepath.Join(base, "FlexTCI")
}

func Load() *Store { return LoadFrom(filepath.Join(Dir(), "config.json")) }

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

// Update applies f to the config and saves it.
func (s *Store) Update(f func(c *Config)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	f(s.cfg)
	s.cfg.normalize()
	_ = os.MkdirAll(filepath.Dir(s.path), 0o755)
	b, _ := json.MarshalIndent(s.cfg, "", "  ")
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
