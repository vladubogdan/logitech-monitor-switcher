//go:build darwin

// Package autostart enables/disables launching the app at login. On macOS this
// is a per-user LaunchAgent plist that runs the current executable at login.
package autostart

import (
	"fmt"
	"os"
	"path/filepath"
)

const label = "dev.local.logimonitorswitch"

func plistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", label+".plist"), nil
}

// IsEnabled reports whether the LaunchAgent plist exists.
func IsEnabled() bool {
	p, err := plistPath()
	if err != nil {
		return false
	}
	_, err = os.Stat(p)
	return err == nil
}

// Enable writes the LaunchAgent for the current executable and loads it.
func Enable() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	p, err := plistPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	content := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>%s</string>
  <key>ProgramArguments</key>
  <array><string>%s</string></array>
  <key>RunAtLoad</key><true/>
  <key>ProcessType</key><string>Interactive</string>
</dict>
</plist>
`, label, exe)
	// Only write the plist. launchd reads ~/Library/LaunchAgents at login and
	// starts it then (RunAtLoad). We intentionally do NOT `launchctl load` here:
	// with the app already running, loading would spawn a second instance.
	return os.WriteFile(p, []byte(content), 0o644)
}

// Disable removes the LaunchAgent so it won't start at the next login. We do
// not unload a currently-running job: if this instance was itself started by
// launchd, unloading would terminate the app the moment you untick the box.
func Disable() error {
	p, err := plistPath()
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
