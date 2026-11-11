package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"

	"github.com/wailsapp/wails/v2/pkg/runtime"

	"github.com/va3mw/SliceTCI/internal/config"
	"github.com/va3mw/SliceTCI/internal/logx"
)

// InstallState tells the Settings dialog where SliceTCI runs from.
type InstallState struct {
	Version     string `json:"version"`
	Exe         string `json:"exe"`
	InstallPath string `json:"install_path"`
	Installed   bool   `json:"installed"` // running from InstallPath
}

func (a *App) GetInstallState() InstallState {
	exe, _ := os.Executable()
	dst := installedExe()
	return InstallState{Version: version, Exe: exe, InstallPath: dst, Installed: samePath(exe, dst)}
}

// ask shows a Yes/No question and reports whether the operator chose Yes.
func (a *App) ask(title, msg string) bool {
	r, err := runtime.MessageDialog(a.ctx, runtime.MessageDialogOptions{Type: runtime.QuestionDialog, Title: title, Message: msg})
	return err == nil && r == "Yes"
}

// Install copies the running exe to C:\Program Files\SliceTCI (through the
// Windows UAC prompt), creates a desktop shortcut to it and restarts from
// there. It returns "" if the operator cancels.
func (a *App) Install() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	dst := installedExe()
	if samePath(exe, dst) {
		return "SliceTCI is already installed and running from " + filepath.Dir(dst), nil
	}
	if !a.ask("Install SliceTCI", "Install SliceTCI to\n\n"+filepath.Dir(dst)+"\n\n"+
		"Windows asks for administrator permission to write there. Settings and logs stay in "+config.Dir()+". "+
		"A desktop shortcut is created, and SliceTCI restarts from the new location (TCI clients reconnect). "+
		"Updates install there too, with the same prompt.\n\nInstall now?") {
		return "", nil
	}
	if err := placeExe(exe, dst); err != nil {
		a.log.Log(logx.Error, "app", "install failed: %v", err)
		return "", fmt.Errorf("install failed: %w", err)
	}
	a.log.Log(logx.Info, "app", "installed to %s", dst)
	if p, err := createDesktopShortcut(dst); err != nil {
		a.log.Log(logx.Warn, "app", "desktop shortcut failed: %v", err)
	} else {
		a.log.Log(logx.Info, "app", "desktop shortcut created: %s", p)
	}
	return "Installed; restarting from " + filepath.Dir(dst), a.restartAs(dst)
}

// CreateShortcut puts a desktop shortcut to the running exe.
func (a *App) CreateShortcut() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	p, err := createDesktopShortcut(exe)
	if err != nil {
		a.log.Log(logx.Warn, "app", "desktop shortcut failed: %v", err)
		return "", err
	}
	a.log.Log(logx.Info, "app", "desktop shortcut created: %s", p)
	return "Shortcut created: " + p, nil
}

// CheckForUpdates looks for a newer release on GitHub and, if the operator
// agrees, downloads it to the temp folder, checks its SHA-256, swaps it in
// (through the UAC prompt when SliceTCI is in Program Files) and restarts.
// It returns a short result for the UI.
func (a *App) CheckForUpdates() (string, error) {
	info, err := fetchRelease(releasesAPI)
	if err != nil {
		a.log.Log(logx.Warn, "app", "update check failed: %v", err)
		return "", fmt.Errorf("could not check for updates: %w", err)
	}
	if !newerVersion(info.Version, version) {
		return "SliceTCI " + version + " is up to date", nil
	}
	if !a.ask("Update SliceTCI", fmt.Sprintf("SliceTCI %s is available (you have %s).\n\n"+
		"Download and install it now? SliceTCI restarts afterwards (TCI clients reconnect).", info.Version, version)) {
		return "", nil
	}
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	a.log.Log(logx.Info, "app", "downloading SliceTCI %s", info.Version)
	newPath, err := downloadUpdate(info, filepath.Join(os.TempDir(), "SliceTCI-"+info.Version+".exe"))
	if err == nil {
		err = placeExe(newPath, exe)
		os.Remove(newPath)
	}
	if err != nil {
		a.log.Log(logx.Error, "app", "update failed: %v", err)
		return "", fmt.Errorf("update failed: %w", err)
	}
	a.log.Log(logx.Info, "app", "SliceTCI %s installed; restarting", info.Version)
	return "Updated to " + info.Version + "; restarting", a.restartAs(exe)
}

// startupChecks removes an exe left by the last install or update, and
// notes in the log if a newer release exists. It never installs anything.
func (a *App) startupChecks() {
	if exe, err := os.Executable(); err == nil {
		cleanupOldExe(exe) // fails quietly in Program Files; the installer deletes it at reboot
		if removeLegacyExe(exe) {
			a.log.Log(logx.Info, "app", "removed the old copy at %s", legacyExe())
		}
	}
	info, err := fetchRelease(releasesAPI)
	switch {
	case err != nil:
		a.log.Log(logx.Debug, "app", "update check: %v", err)
	case newerVersion(info.Version, version):
		a.log.Log(logx.Info, "app", "SliceTCI %s is available (you have %s): Settings > Check for updates", info.Version, version)
	}
}

// restartAs starts exe and closes this instance. The new process waits for
// this one to exit, since it holds the single-instance lock and TCI ports.
func (a *App) restartAs(exe string) error {
	cmd := exec.Command(exe, waitPIDFlag, strconv.Itoa(os.Getpid()))
	cmd.Dir = filepath.Dir(exe)
	if err := cmd.Start(); err != nil {
		a.log.Log(logx.Error, "app", "could not start %s: %v", exe, err)
		return fmt.Errorf("could not start %s (please start it yourself): %w", exe, err)
	}
	runtime.Quit(a.ctx)
	return nil
}
