//go:build darwin || linux

// Package singleton ensures only one instance of the app runs per user.
package singleton

import (
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

var lockFile *os.File

// Acquire returns true if this is the only running instance. It holds an
// exclusive advisory lock on a file in the user config dir for the process
// lifetime; a second instance fails to take the lock and gets false.
func Acquire() bool {
	dir, err := os.UserConfigDir()
	if err != nil {
		return true // fail open: don't block startup if we can't check
	}
	appDir := filepath.Join(dir, "logiMonitorSwitch")
	if err := os.MkdirAll(appDir, 0o755); err != nil {
		return true
	}
	f, err := os.OpenFile(filepath.Join(appDir, "instance.lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return true
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		return false // another instance holds the lock
	}
	lockFile = f // keep open (and locked) for the process lifetime
	return true
}
