package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSetIniKeys(t *testing.T) {
	in := "[General]\r\nx=1\r\n\r\n[Configuration]\r\nMyCall=VA3MW\r\nRig=FlexRadio 6xxx\r\nTCIAudio=false\r\n\r\n[Common]\r\nMode=FT8\r\n"
	out := setIniKeys(in, "Configuration", wsjtxTCISettings("50002"))
	if strings.Contains(strings.ReplaceAll(out, "\r\n", ""), "\n") {
		t.Fatal("line endings changed")
	}
	for k, want := range map[string]string{
		"MyCall": "VA3MW", "Rig": "TCI Client RX1", "CATTCIPort": "127.0.0.1:50002",
		"TCIAudio": "true", "PTTMethod": wsjtxPTTCAT, "SplitMode": wsjtxSplitNone,
	} {
		if got := iniValue(out, "Configuration", k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if iniValue(out, "Common", "Mode") != "FT8" || iniValue(out, "General", "x") != "1" {
		t.Fatal("other sections changed")
	}
	if strings.Count(out, "Rig=") != 1 {
		t.Fatal("Rig duplicated")
	}
	// New keys stay inside [Configuration], before [Common].
	if strings.Index(out, "CATTCIPort=") > strings.Index(out, "[Common]") {
		t.Fatal("added keys landed in the wrong section")
	}

	// No file at all: a [Configuration] section is created.
	out = setIniKeys("", "Configuration", wsjtxTCISettings("50001"))
	if !strings.HasPrefix(out, "[Configuration]\n") || iniValue(out, "Configuration", "CATTCIPort") != "127.0.0.1:50001" {
		t.Fatalf("bad new file:\n%s", out)
	}
}

func TestPrepareWSJTXIni(t *testing.T) {
	lad := t.TempDir()
	main := wsjtxIniPath(lad, "")
	os.MkdirAll(filepath.Dir(main), 0o755)
	os.WriteFile(main, []byte("[Configuration]\nMyCall=K0VM\nRig=None\n"), 0o644)

	ok, err := prepareWSJTXIni(lad, "SliceB-TCI", "50002")
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	p := filepath.Join(lad, "WSJT-X - SliceB-TCI", "WSJT-X - SliceB-TCI.ini")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if iniValue(string(b), "Configuration", "MyCall") != "K0VM" || iniValue(string(b), "Configuration", "CATTCIPort") != "127.0.0.1:50002" {
		t.Fatalf("bad settings:\n%s", b)
	}
	if mb, _ := os.ReadFile(main); !strings.Contains(string(mb), "Rig=None") {
		t.Fatal("main WSJT-X settings were changed")
	}

	// An existing copy is left alone.
	os.WriteFile(p, []byte("[Configuration]\nRig=Mine\n"), 0o644)
	if ok, err := prepareWSJTXIni(lad, "SliceB-TCI", "50009"); ok || err != nil {
		t.Fatal("existing settings were rewritten")
	}
	if b, _ := os.ReadFile(p); string(b) != "[Configuration]\nRig=Mine\n" {
		t.Fatal("existing settings changed")
	}
}
