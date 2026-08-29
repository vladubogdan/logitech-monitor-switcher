//go:build windows

// Package singleton ensures only one instance of the app runs per user.
package singleton

import (
	"golang.org/x/sys/windows"
)

var mutex windows.Handle

// Acquire returns true if this is the only running instance, using a named
// mutex. A second instance sees ERROR_ALREADY_EXISTS and gets false.
func Acquire() bool {
	name, err := windows.UTF16PtrFromString("Local\\logiMonitorSwitch.singleton")
	if err != nil {
		return true
	}
	h, err := windows.CreateMutex(nil, false, name)
	if h == 0 {
		return true // fail open
	}
	if err == windows.ERROR_ALREADY_EXISTS {
		return false
	}
	mutex = h // keep the handle for the process lifetime
	return true
}
