package main

import (
	"os"
	"path/filepath"
	"strings"
)

// WSJT-X desktop shortcuts: one per SliceTCI channel, each starting WSJT-X
// with its own --rig-name so the copies keep separate settings. A copy
// started with --rig-name=X keeps them in
// %LOCALAPPDATA%\WSJT-X - X\WSJT-X - X.ini.

const wsjtxExe = "wsjtx.exe"

func wsjtxRigName(slice string) string      { return "Slice" + slice + "-TCI" }
func wsjtxShortcutName(slice string) string { return "WSJT-X " + slice + " TCI" }

// wsjtxIniPath is the settings file of the WSJT-X copy started with
// --rig-name=rig ("" is the main copy).
func wsjtxIniPath(localAppData, rig string) string {
	name := "WSJT-X"
	if rig != "" {
		name += " - " + rig
	}
	return filepath.Join(localAppData, name, name+".ini")
}

// QSettings encodings of the enum values WSJT-X stores, copied from its own
// settings files.
const (
	wsjtxPTTCAT    = `@Variant(\0\0\0\x7f\0\0\0\x1eTransceiverFactory::PTTMethod\0\0\0\0\xfPTT_method_CAT\0)`
	wsjtxDataNone  = `@Variant(\0\0\0\x7f\0\0\0\x18\x43onfiguration::DataMode\0\0\0\0\xf\x64\x61ta_mode_none\0)`
	wsjtxSplitNone = `@Variant(\0\0\0\x7f\0\0\0\x1eTransceiverFactory::SplitMode\0\0\0\0\x10split_mode_none\0)`
)

// wsjtxTCISettings are the [Configuration] values for a WSJT-X copy on a
// SliceTCI port: TCI rig and audio, PTT over CAT, Mode and Split None.
func wsjtxTCISettings(port string) [][2]string {
	return [][2]string{
		{"Rig", "TCI Client RX1"},
		{"CATTCIPort", "127.0.0.1:" + port},
		{"TCIAudio", "true"},
		{"PTTMethod", wsjtxPTTCAT},
		{"DataMode", wsjtxDataNone},
		{"SplitMode", wsjtxSplitNone},
	}
}

// iniEOL is the line ending the file already uses.
func iniEOL(content string) string {
	if strings.Contains(content, "\r\n") {
		return "\r\n"
	}
	return "\n"
}

// setIniKeys sets keys in [section] of an ini text, replacing existing
// lines and adding missing ones at the end of the section (or a new
// section at the end). Everything else is kept as it was.
func setIniKeys(content, section string, kv [][2]string) string {
	eol := iniEOL(content)
	lines := strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
	header := "[" + section + "]"
	start, end := -1, len(lines)
	for i, l := range lines {
		t := strings.TrimSpace(l)
		if start < 0 && t == header {
			start = i
		} else if start >= 0 && strings.HasPrefix(t, "[") {
			end = i
			break
		}
	}
	if start < 0 {
		for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
			lines = lines[:len(lines)-1]
		}
		if len(lines) > 0 {
			lines = append(lines, "")
		}
		lines = append(lines, header)
		start, end = len(lines)-1, len(lines)
	}
	var missing []string
	for _, p := range kv {
		found := false
		for i := start + 1; i < end; i++ {
			if k, _, ok := strings.Cut(lines[i], "="); ok && strings.TrimSpace(k) == p[0] {
				lines[i] = p[0] + "=" + p[1]
				found = true
			}
		}
		if !found {
			missing = append(missing, p[0]+"="+p[1])
		}
	}
	// Insert before the blank lines that end the section.
	at := end
	for at > start+1 && strings.TrimSpace(lines[at-1]) == "" {
		at--
	}
	out := append(append(append([]string{}, lines[:at]...), missing...), lines[at:]...)
	s := strings.Join(out, "\n")
	if !strings.HasSuffix(s, "\n") {
		s += "\n"
	}
	return strings.ReplaceAll(s, "\n", eol)
}

// iniValue reads key from [section]; "" if absent.
func iniValue(content, section, key string) string {
	in := false
	for _, l := range strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n") {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "[") {
			in = t == "["+section+"]"
			continue
		}
		if k, v, ok := strings.Cut(l, "="); in && ok && strings.TrimSpace(k) == key {
			return v
		}
	}
	return ""
}

// prepareWSJTXIni creates the settings file for a new WSJT-X copy on port.
// It starts from the main copy's settings (callsign, grid and the rest) when
// there are any. It returns false, and changes nothing, if the copy already
// has settings.
func prepareWSJTXIni(localAppData, rig, port string) (bool, error) {
	dst := wsjtxIniPath(localAppData, rig)
	if _, err := os.Stat(dst); err == nil {
		return false, nil
	}
	base := ""
	if b, err := os.ReadFile(wsjtxIniPath(localAppData, "")); err == nil {
		base = string(b)
	}
	out := setIniKeys(base, "Configuration", wsjtxTCISettings(port))
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return false, err
	}
	return true, os.WriteFile(dst, []byte(out), 0o644)
}

// wsjtxCommonPaths are the usual install spots; the installer's default is
// C:\WSJT\wsjtx.
func wsjtxCommonPaths() []string {
	var ps []string
	for _, root := range []string{`C:\WSJT`, os.Getenv("ProgramW6432"), os.Getenv("ProgramFiles"), os.Getenv("ProgramFiles(x86)")} {
		if root != "" {
			ps = append(ps, filepath.Join(root, "wsjtx", "bin", wsjtxExe))
		}
	}
	return ps
}

// existingUnique keeps the paths that exist, once each.
func existingUnique(paths []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, p := range paths {
		if p == "" {
			continue
		}
		k := strings.ToLower(filepath.Clean(p))
		if seen[k] {
			continue
		}
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			seen[k] = true
			out = append(out, p)
		}
	}
	return out
}
