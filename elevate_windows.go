package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"unsafe"
)

var (
	shell32             = syscall.NewLazyDLL("shell32.dll")
	procShellExecuteExW = shell32.NewProc("ShellExecuteExW")
	kernel32            = syscall.NewLazyDLL("kernel32.dll")
	procMoveFileExW     = kernel32.NewProc("MoveFileExW")
)

// shellExecuteInfo is SHELLEXECUTEINFOW.
type shellExecuteInfo struct {
	cbSize       uint32
	fMask        uint32
	hwnd         uintptr
	lpVerb       *uint16
	lpFile       *uint16
	lpParameters *uint16
	lpDirectory  *uint16
	nShow        int32
	hInstApp     uintptr
	lpIDList     uintptr
	lpClass      *uint16
	hkeyClass    uintptr
	dwHotKey     uint32
	hIcon        uintptr
	hProcess     syscall.Handle
}

const (
	seeMaskNoCloseProcess    = 0x00000040
	seeMaskNoAsync           = 0x00000100
	swHideCmd                = 0
	errorCancelled           = syscall.Errno(1223)
	moveFileDelayUntilReboot = 0x4
)

var errElevationCancelled = errors.New("Windows administrator permission was not given")

// quoteArg quotes one command-line argument the way CommandLineToArgvW reads it.
func quoteArg(s string) string { return syscall.EscapeArg(s) }

// runElevated starts exe with args through the UAC prompt ("runas") and
// waits for it to exit, returning its exit code.
func runElevated(exe string, args ...string) (uint32, error) {
	params := ""
	for i, a := range args {
		if i > 0 {
			params += " "
		}
		params += quoteArg(a)
	}
	verb, _ := syscall.UTF16PtrFromString("runas")
	file, _ := syscall.UTF16PtrFromString(exe)
	par, _ := syscall.UTF16PtrFromString(params)
	dir, _ := syscall.UTF16PtrFromString(filepath.Dir(exe))
	info := shellExecuteInfo{
		fMask:        seeMaskNoCloseProcess | seeMaskNoAsync,
		lpVerb:       verb,
		lpFile:       file,
		lpParameters: par,
		lpDirectory:  dir,
		nShow:        swHideCmd,
	}
	info.cbSize = uint32(unsafe.Sizeof(info))
	if ok, _, err := procShellExecuteExW.Call(uintptr(unsafe.Pointer(&info))); ok == 0 {
		if errors.Is(err, errorCancelled) {
			return 0, errElevationCancelled
		}
		return 0, fmt.Errorf("could not start the elevated installer: %w", err)
	}
	if info.hProcess == 0 {
		return 0, errors.New("the elevated installer did not start")
	}
	defer syscall.CloseHandle(info.hProcess)
	if _, err := syscall.WaitForSingleObject(info.hProcess, syscall.INFINITE); err != nil {
		return 0, err
	}
	var code uint32
	if err := syscall.GetExitCodeProcess(info.hProcess, &code); err != nil {
		return 0, err
	}
	return code, nil
}

// deleteAtReboot asks Windows to remove path at the next restart, for an
// .old exe that is still running and so cannot be deleted now.
func deleteAtReboot(path string) {
	if _, err := os.Stat(path); err != nil {
		return
	}
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return
	}
	procMoveFileExW.Call(uintptr(unsafe.Pointer(p)), 0, moveFileDelayUntilReboot)
}

// elevatedInstallFlag runs SliceTCI as the elevated installer:
// SliceTCI.exe -elevated-install <src> <dst> <result file>.
const elevatedInstallFlag = "-elevated-install"

// installElevated runs installTo(src, dst) in an elevated copy of the
// running exe and reports its error, if any.
func installElevated(src, dst string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	f, err := os.CreateTemp("", "SliceTCI-install-*.txt")
	if err != nil {
		return err
	}
	result := f.Name()
	f.Close()
	defer os.Remove(result)
	code, err := runElevated(self, elevatedInstallFlag, src, dst, result)
	if err != nil {
		return err
	}
	if code != 0 {
		msg, _ := os.ReadFile(result)
		if len(msg) == 0 {
			msg = []byte(fmt.Sprintf("the elevated installer exited with code %d", code))
		}
		return errors.New(string(msg))
	}
	return nil
}

// runElevatedInstall is the elevated side of installElevated. It returns the
// process exit code.
func runElevatedInstall(args []string) int {
	if len(args) != 3 {
		return 2
	}
	src, dst, result := args[0], args[1], args[2]
	if err := installTo(src, dst); err != nil {
		os.WriteFile(result, []byte(err.Error()), 0o644)
		return 1
	}
	deleteAtReboot(dst + ".old") // the previous exe may still be running
	return 0
}
