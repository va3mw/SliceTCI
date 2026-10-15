package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoadFlatOptions: config.json keeps the options flat, as files written
// before Options was split out do.
func TestLoadFlatOptions(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	old := `{"radio_ip":"10.0.0.5","channel_count":1,"unkey_timeout_ms":1500,"max_tx_seconds":60,"protocol_name":"Thetis","spots":false,"log_traffic":true}`
	if err := os.WriteFile(p, []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	c := LoadFrom(p).Get()
	if c.RadioIP != "10.0.0.5" || c.UnkeyTimeoutMs != 1500 || c.MaxTXSeconds != 60 ||
		c.ProtocolName != "Thetis" || c.Spots || !c.LogTraffic {
		t.Fatalf("options not loaded: %+v", c)
	}
	if !c.SetSliceDAX || !c.DigitalOnTX {
		t.Fatalf("missing options did not keep their defaults: %+v", c.Options)
	}
}
