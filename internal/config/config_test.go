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

// TestMigrateLegacyDir: settings from before the rename (FlexTCI) move to
// the SliceTCI folder, and an existing SliceTCI folder is left alone.
func TestMigrateLegacyDir(t *testing.T) {
	base := t.TempDir()
	old := filepath.Join(base, legacyDirName)
	dir := filepath.Join(base, "SliceTCI")
	if err := os.MkdirAll(filepath.Join(old, "logs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(old, "config.json"), []byte(`{"radio_ip":"10.0.0.7"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	migrateLegacyDir(dir)
	if c := LoadFrom(filepath.Join(dir, "config.json")).Get(); c.RadioIP != "10.0.0.7" {
		t.Fatalf("settings not migrated: %+v", c)
	}
	if _, err := os.Stat(filepath.Join(dir, "logs")); err != nil {
		t.Fatalf("logs folder not moved: %v", err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("old folder still there: %v", err)
	}

	// A second legacy folder appearing later must not overwrite the new one.
	if err := os.MkdirAll(old, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(old, "config.json"), []byte(`{"radio_ip":"10.0.0.9"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	migrateLegacyDir(dir)
	if c := LoadFrom(filepath.Join(dir, "config.json")).Get(); c.RadioIP != "10.0.0.7" {
		t.Fatalf("existing SliceTCI settings overwritten: %+v", c)
	}
}
