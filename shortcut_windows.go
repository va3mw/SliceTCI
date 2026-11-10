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
// returns its path. It uses the WScript.Shell COM object via PowerShell; the
// paths go in through environment variables so no quoting is needed.
func createDesktopShortcut(exe string) (string, error) {
	script := `$ErrorActionPreference = 'Stop'
$d = [Environment]::GetFolderPath('Desktop')
$p = Join-Path $d 'SliceTCI.lnk'
$s = (New-Object -ComObject WScript.Shell).CreateShortcut($p)
$s.TargetPath = $env:FT_TARGET
$s.WorkingDirectory = $env:FT_DIR
$s.IconLocation = $env:FT_TARGET + ',0'
$s.Description = 'SliceTCI'
$s.Save()
Write-Output $p`
	cmd := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", script)
	cmd.Env = append(os.Environ(), "FT_TARGET="+exe, "FT_DIR="+filepath.Dir(exe))
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}
