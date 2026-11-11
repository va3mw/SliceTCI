package main

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/va3mw/SliceTCI/internal/config"
)

const exeName = "SliceTCI.exe"

// installDir is where Install puts SliceTCI: C:\Program Files\SliceTCI on
// Windows. Settings and logs stay in %APPDATA%\SliceTCI.
func installDir() string {
	if runtime.GOOS != "windows" {
		return config.Dir()
	}
	for _, v := range []string{"ProgramW6432", "ProgramFiles"} {
		if d := os.Getenv(v); d != "" {
			return filepath.Join(d, "SliceTCI")
		}
	}
	return `C:\Program Files\SliceTCI`
}

func installedExe() string { return filepath.Join(installDir(), exeName) }

// legacyExe is where Install put SliceTCI up to 0.6.2, next to the settings.
func legacyExe() string { return filepath.Join(config.Dir(), exeName) }

// placeExe puts src at dst like installTo. If dst's folder needs
// administrator rights (Program Files), it runs installTo in an elevated
// copy of SliceTCI, which shows the Windows UAC prompt.
func placeExe(src, dst string) error {
	err := installTo(src, dst)
	if err == nil || !errors.Is(err, fs.ErrPermission) {
		return err
	}
	return installElevated(src, dst)
}

// removeLegacyExe deletes the exe an older Install left in the settings
// folder, once SliceTCI runs from the new install folder.
func removeLegacyExe(running string) bool {
	old := legacyExe()
	if samePath(running, old) || !samePath(running, installedExe()) {
		return false
	}
	os.Remove(old + ".old")
	return os.Remove(old) == nil
}

func samePath(a, b string) bool {
	ca, errA := filepath.Abs(a)
	cb, errB := filepath.Abs(b)
	return errA == nil && errB == nil && strings.EqualFold(filepath.Clean(ca), filepath.Clean(cb))
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(dst)
	}
	return err
}

// installTo copies the running exe to dst. An exe already at dst is moved
// aside to .old first, since it may be running.
func installTo(srcExe, dst string) error {
	if samePath(srcExe, dst) {
		return errors.New("SliceTCI is already running from " + filepath.Dir(dst))
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp := dst + ".new"
	if err := copyFile(srcExe, tmp); err != nil {
		return err
	}
	if _, err := os.Stat(dst); err != nil {
		if err := os.Rename(tmp, dst); err != nil {
			os.Remove(tmp)
			return err
		}
		return nil
	}
	if err := swapExe(dst, tmp); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// swapExe puts newPath in place of exePath. A running exe cannot be
// overwritten on Windows but can be renamed, so the old one becomes
// <exe>.old and is deleted at the next start.
func swapExe(exePath, newPath string) error {
	old := exePath + ".old"
	os.Remove(old)
	if err := os.Rename(exePath, old); err != nil {
		return err
	}
	if err := os.Rename(newPath, exePath); err != nil {
		os.Rename(old, exePath) // put the original back
		return err
	}
	return nil
}

// cleanupOldExe removes the previous exe left by an install or update.
func cleanupOldExe(exePath string) { os.Remove(exePath + ".old") }

// waitForExit is used by a restarted SliceTCI (started with -wait-pid) to let
// the previous instance close first: it holds the single-instance lock and
// the TCI ports. It gives up after timeout.
func waitForExit(pid int, timeout time.Duration) {
	p, err := os.FindProcess(pid)
	if err != nil {
		return
	}
	done := make(chan struct{})
	go func() {
		p.Wait() // on Windows this waits for any process, not only children
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
	}
}
