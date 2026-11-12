package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/wailsapp/wails/v2/pkg/runtime"

	"github.com/va3mw/SliceTCI/internal/config"
	"github.com/va3mw/SliceTCI/internal/logx"
)

// WSJTXState tells the Settings dialog which wsjtx.exe the shortcuts use.
type WSJTXState struct {
	Path string `json:"path"` // "" until found or picked
}

func (a *App) GetWSJTXState() WSJTXState {
	p := a.cfg.Get().WSJTXPath
	if len(existingUnique([]string{p})) == 0 {
		p = ""
	}
	return WSJTXState{Path: p}
}

// PickWSJTX lets the operator choose wsjtx.exe and remembers it. It returns
// "" if the operator cancels.
func (a *App) PickWSJTX() (string, error) {
	start := `C:\`
	if c := a.wsjtxCandidates(); len(c) > 0 {
		start = filepath.Dir(c[0])
	}
	p, err := runtime.OpenFileDialog(a.ctx, runtime.OpenDialogOptions{
		Title:            "Find wsjtx.exe (usually C:\\WSJT\\wsjtx\\bin)",
		DefaultDirectory: start,
		Filters:          []runtime.FileFilter{{DisplayName: "WSJT-X (wsjtx.exe)", Pattern: "wsjtx.exe"}},
	})
	if err != nil || p == "" {
		return "", err
	}
	if !strings.EqualFold(filepath.Base(p), wsjtxExe) {
		return "", errors.New("please pick wsjtx.exe, in the bin folder of WSJT-X")
	}
	if err := a.save(func(c *config.Config) { c.WSJTXPath = p }); err != nil {
		return "", err
	}
	a.log.Log(logx.Info, "app", "WSJT-X: using %s", p)
	return "Using " + p, nil
}

func (a *App) wsjtxCandidates() []string {
	return existingUnique(append(wsjtxFromRegistry(), wsjtxCommonPaths()...))
}

// findWSJTX returns the saved wsjtx.exe, else the only one installed, else
// the one the operator picks ("" if cancelled).
func (a *App) findWSJTX() (string, error) {
	if s := a.GetWSJTXState(); s.Path != "" {
		return s.Path, nil
	}
	if c := a.wsjtxCandidates(); len(c) == 1 {
		a.log.Log(logx.Info, "app", "WSJT-X found at %s", c[0])
		return c[0], a.save(func(cf *config.Config) { cf.WSJTXPath = c[0] })
	}
	if _, err := a.PickWSJTX(); err != nil {
		return "", err
	}
	return a.GetWSJTXState().Path, nil
}

// CreateWSJTXShortcuts puts a desktop shortcut for each channel that starts
// WSJT-X with its own --rig-name, and sets up the TCI settings of copies
// that have none yet. It returns a short result for the UI, "" if cancelled.
func (a *App) CreateWSJTXShortcuts() (string, error) {
	exe, err := a.findWSJTX()
	if err != nil || exe == "" {
		return "", err
	}
	lad := os.Getenv("LOCALAPPDATA")
	c := a.cfg.Get()
	var made, prepared, kept, warn []string
	for i := 0; i < c.ChannelCount; i++ {
		ch := c.Channels[i]
		rig, port := wsjtxRigName(ch.Slice), strconv.Itoa(ch.Port)
		if _, err := makeDesktopShortcut(wsjtxShortcutName(ch.Slice), exe, "--rig-name="+rig,
			"WSJT-X on SliceTCI Slice "+ch.Slice+" (port "+port+")"); err != nil {
			a.log.Log(logx.Warn, "app", "WSJT-X shortcut for Slice %s failed: %v", ch.Slice, err)
			return "", fmt.Errorf("could not create the shortcut for Slice %s: %w", ch.Slice, err)
		}
		made = append(made, ch.Slice)
		if lad == "" {
			continue
		}
		ok, err := prepareWSJTXIni(lad, rig, port)
		switch {
		case err != nil:
			a.log.Log(logx.Warn, "app", "WSJT-X settings for %s: %v", rig, err)
			warn = append(warn, fmt.Sprintf("Slice %s: could not write its WSJT-X settings (%v).", ch.Slice, err))
		case ok:
			prepared = append(prepared, ch.Slice)
		default:
			kept = append(kept, ch.Slice)
			if b, err := os.ReadFile(wsjtxIniPath(lad, rig)); err == nil {
				if got := iniValue(string(b), "Configuration", "CATTCIPort"); got != "127.0.0.1:"+port {
					if got == "" {
						got = "no TCI server"
					}
					warn = append(warn, fmt.Sprintf("Slice %s: its WSJT-X uses %s, but the channel is on port %s. Fix it in WSJT-X Settings > Radio.", ch.Slice, got, port))
				}
			}
		}
	}
	a.log.Log(logx.Info, "app", "WSJT-X shortcuts created for Slice %s (%s); new settings for %v",
		strings.Join(made, ", "), exe, prepared)

	var b strings.Builder
	fmt.Fprintf(&b, "Desktop shortcuts created: %s.\n\nThey start %s\n", list("WSJT-X ", " TCI", made), exe)
	if len(prepared) > 0 {
		fmt.Fprintf(&b, "\nSet up for TCI (rig, port, TCI audio, PTT CAT), copying your callsign and other settings from your main WSJT-X: Slice %s.\n", strings.Join(prepared, ", "))
	}
	if len(kept) > 0 {
		fmt.Fprintf(&b, "\nAlready had their own settings, left as they were: Slice %s.\n", strings.Join(kept, ", "))
	}
	for _, w := range warn {
		b.WriteString("\n" + w + "\n")
	}
	runtime.MessageDialog(a.ctx, runtime.MessageDialogOptions{Type: runtime.InfoDialog, Title: "WSJT-X shortcuts", Message: b.String()})
	return fmt.Sprintf("%d WSJT-X shortcut(s) on the desktop", len(made)), nil
}

func list(pre, post string, xs []string) string {
	out := make([]string, len(xs))
	for i, x := range xs {
		out[i] = pre + x + post
	}
	return strings.Join(out, ", ")
}
