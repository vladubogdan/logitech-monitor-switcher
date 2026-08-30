//go:build windows

package powermon

import (
	"runtime"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	wmPowerBroadcast      = 0x0218
	pbtAPMSuspend         = 0x0004
	pbtAPMResumeSuspend   = 0x0007
	pbtAPMResumeAutomatic = 0x0012
)

var (
	user32               = windows.NewLazySystemDLL("user32.dll")
	procRegisterClassW   = user32.NewProc("RegisterClassW")
	procCreateWindowExW  = user32.NewProc("CreateWindowExW")
	procDefWindowProcW   = user32.NewProc("DefWindowProcW")
	procGetMessageW      = user32.NewProc("GetMessageW")
	procTranslateMessage = user32.NewProc("TranslateMessage")
	procDispatchMessageW = user32.NewProc("DispatchMessageW")
)

// wndClassW mirrors the Win32 WNDCLASSW structure.
type wndClassW struct {
	style         uint32
	lpfnWndProc   uintptr
	cbClsExtra    int32
	cbWndExtra    int32
	hInstance     windows.Handle
	hIcon         windows.Handle
	hCursor       windows.Handle
	hbrBackground windows.Handle
	lpszMenuName  *uint16
	lpszClassName *uint16
}

// msgW mirrors the Win32 MSG structure.
type msgW struct {
	hwnd    windows.Handle
	message uint32
	wParam  uintptr
	lParam  uintptr
	time    uint32
	pt      struct{ x, y int32 }
}

func wndProc(hwnd windows.Handle, message uint32, wParam, lParam uintptr) uintptr {
	if message == wmPowerBroadcast {
		switch wParam {
		case pbtAPMSuspend:
			emit(Sleep)
		case pbtAPMResumeSuspend, pbtAPMResumeAutomatic:
			emit(Wake)
		}
		return 1 // TRUE
	}
	ret, _, _ := procDefWindowProcW.Call(uintptr(hwnd), uintptr(message), wParam, lParam)
	return ret
}

var startOnce sync.Once

// Start creates a hidden top-level message window and pumps its messages on a
// dedicated OS thread so it receives WM_POWERBROADCAST. Idempotent.
//
// Note: a normal (never-shown) top-level window is used rather than a
// message-only (HWND_MESSAGE) window, because broadcast messages such as
// WM_POWERBROADCAST are not delivered to message-only windows.
func Start() {
	startOnce.Do(func() {
		go messageLoop()
	})
}

func messageLoop() {
	// A window's messages are delivered to the thread that created it, so keep
	// this goroutine on one OS thread for the lifetime of the message pump.
	runtime.LockOSThread()

	className, err := windows.UTF16PtrFromString("logiMonitorSwitchPowerWnd")
	if err != nil {
		return
	}

	wc := wndClassW{
		lpfnWndProc:   syscall.NewCallback(wndProc),
		lpszClassName: className,
	}
	if atom, _, _ := procRegisterClassW.Call(uintptr(unsafe.Pointer(&wc))); atom == 0 {
		return
	}

	hwnd, _, _ := procCreateWindowExW.Call(
		0,                                  // dwExStyle
		uintptr(unsafe.Pointer(className)), // lpClassName
		uintptr(unsafe.Pointer(className)), // lpWindowName
		0,                                  // dwStyle (not visible)
		0, 0, 0, 0,                         // x, y, w, h
		0, // hWndParent (top-level, so it gets broadcasts)
		0, // hMenu
		0, // hInstance
		0, // lpParam
	)
	if hwnd == 0 {
		return
	}

	var m msgW
	for {
		r, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(r) <= 0 { // 0 == WM_QUIT, -1 == error
			return
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}
}
