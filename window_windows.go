package main

import (
	"os"
	"strings"
	"syscall"
	"unsafe"

	"github.com/va3mw/SliceTCI/internal/config"
)

// Window placement uses Win32 directly: Wails' WindowGetPosition and
// WindowSetPosition are relative to the window's current monitor, so they
// cannot put the window back on a second screen.

var (
	user32                  = syscall.NewLazyDLL("user32.dll")
	procEnumWindows         = user32.NewProc("EnumWindows")
	procGetWindowThreadPID  = user32.NewProc("GetWindowThreadProcessId")
	procGetWindowTextW      = user32.NewProc("GetWindowTextW")
	procGetWindow           = user32.NewProc("GetWindow")
	procIsWindow            = user32.NewProc("IsWindow")
	procGetWindowPlacement  = user32.NewProc("GetWindowPlacement")
	procSetWindowPlacement  = user32.NewProc("SetWindowPlacement")
	procMonitorFromRect     = user32.NewProc("MonitorFromRect")
	enumWindowsCallbackFunc = syscall.NewCallback(enumWindowsCallback)
	enumFound               syscall.Handle
)

type rect struct{ Left, Top, Right, Bottom int32 }
type point struct{ X, Y int32 }

type windowPlacement struct {
	Length     uint32
	Flags      uint32
	ShowCmd    uint32
	MinPos     point
	MaxPos     point
	NormalRect rect
}

const (
	gwOwner              = 4
	swHide               = 0
	monitorDefaultToNull = 0
)

func enumWindowsCallback(hwnd syscall.Handle, _ uintptr) uintptr {
	var pid uint32
	procGetWindowThreadPID.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&pid)))
	if int(pid) != os.Getpid() {
		return 1
	}
	if owner, _, _ := procGetWindow.Call(uintptr(hwnd), gwOwner); owner != 0 {
		return 1
	}
	buf := make([]uint16, 128)
	n, _, _ := procGetWindowTextW.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if strings.HasPrefix(syscall.UTF16ToString(buf[:n]), "SliceTCI") {
		enumFound = hwnd
		return 0
	}
	return 1
}

// mainWindow finds SliceTCI's top-level window in this process.
func mainWindow() syscall.Handle {
	enumFound = 0
	procEnumWindows.Call(enumWindowsCallbackFunc, 0)
	return enumFound
}

func (a *App) windowHandle() syscall.Handle {
	if a.hwnd != 0 {
		if ok, _, _ := procIsWindow.Call(a.hwnd); ok != 0 {
			return syscall.Handle(a.hwnd)
		}
	}
	h := mainWindow()
	a.hwnd = uintptr(h)
	return h
}

// restoreWindow moves the (still hidden) window to where it was last
// closed, if that spot is still on a connected monitor.
func (a *App) restoreWindow() {
	w := a.cfg.Get().Window
	if w == nil || w.Right-w.Left < 100 || w.Bottom-w.Top < 100 {
		return
	}
	hwnd := a.windowHandle()
	if hwnd == 0 {
		return
	}
	// The title bar must be on a monitor, or the window could not be dragged back.
	title := rect{w.Left + 40, w.Top, w.Right - 40, w.Top + 30}
	if m, _, _ := procMonitorFromRect.Call(uintptr(unsafe.Pointer(&title)), monitorDefaultToNull); m == 0 {
		return
	}
	wp := windowPlacement{Length: uint32(unsafe.Sizeof(windowPlacement{}))}
	if ok, _, _ := procGetWindowPlacement.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&wp))); ok == 0 {
		return
	}
	wp.Flags = 0
	wp.ShowCmd = swHide // keep it hidden; WindowShow follows
	wp.NormalRect = rect{w.Left, w.Top, w.Right, w.Bottom}
	procSetWindowPlacement.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&wp)))
}

// saveWindow records the window's normal position (also correct when it is
// minimised or maximised at close).
func (a *App) saveWindow() {
	hwnd := a.windowHandle()
	if hwnd == 0 {
		return
	}
	wp := windowPlacement{Length: uint32(unsafe.Sizeof(windowPlacement{}))}
	if ok, _, _ := procGetWindowPlacement.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&wp))); ok == 0 {
		return
	}
	r := wp.NormalRect
	if r.Right-r.Left < 100 || r.Bottom-r.Top < 100 {
		return
	}
	nw := &config.WindowRect{Left: r.Left, Top: r.Top, Right: r.Right, Bottom: r.Bottom}
	if old := a.cfg.Get().Window; old != nil && *old == *nw {
		return
	}
	a.cfg.Update(func(c *config.Config) { c.Window = nw })
}
