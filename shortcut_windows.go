package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

const createNoWindow = 0x08000000

// createDesktopShortcut puts SliceTCI.lnk on the current user's desktop
// (wherever Windows has it, including a OneDrive-redirected desktop) and
// returns its path.
func createDesktopShortcut(exe string) (string, error) {
	return makeDesktopShortcut("SliceTCI", exe, "", "SliceTCI")
}

// makeDesktopShortcut writes <name>.lnk on the desktop, starting target with
// args in target's folder, with target's own icon. It uses the WScript.Shell
// COM object via PowerShell; the values go in through environment variables
// so no quoting is needed. An existing shortcut of that name is replaced.
func makeDesktopShortcut(name, target, args, desc string) (string, error) {
	script := `$ErrorActionPreference = 'Stop'
$d = [Environment]::GetFolderPath('Desktop')
$p = Join-Path $d ($env:FT_NAME + '.lnk')
$s = (New-Object -ComObject WScript.Shell).CreateShortcut($p)
$s.TargetPath = $env:FT_TARGET
$s.Arguments = $env:FT_ARGS
$s.WorkingDirectory = $env:FT_DIR
$s.IconLocation = $env:FT_TARGET + ',0'
$s.Description = $env:FT_DESC
$s.Save()
Write-Output $p`
	cmd := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", script)
	cmd.Env = append(os.Environ(), "FT_NAME="+name, "FT_TARGET="+target, "FT_ARGS="+args,
		"FT_DIR="+filepath.Dir(target), "FT_DESC="+desc)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}
