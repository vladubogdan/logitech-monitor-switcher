// Package config loads and persists user settings for logiMonitorSwitch.
//
// Settings live in a single JSON file in the OS user-config dir:
//
//	macOS:   ~/Library/Application Support/logiMonitorSwitch/config.json
//	Windows: %AppData%\logiMonitorSwitch\config.json
package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// TriggerDevice selects which device leaving this host starts a monitor switch.
type TriggerDevice string

const (
	// TriggerKeyboard fires when the keyboard roams off this host. This is the
	// right default: in both Logitech modes (Flow-drag and number-key) the
	// keyboard always leaves, whereas the mouse only leaves in Flow mode.
	TriggerKeyboard TriggerDevice = "keyboard"
	// TriggerMouse fires when the mouse roams off this host.
	TriggerMouse TriggerDevice = "mouse"
	// TriggerEither fires when either device roams off.
	TriggerEither TriggerDevice = "either"
)

// Config is the full persisted settings document.
type Config struct {
	// Enabled is the master on/off switch for automatic switching.
	Enabled bool `json:"enabled"`

	// TriggerDevice decides which roam-away event triggers a switch.
	Trigger TriggerDevice `json:"trigger"`

	// MonitorTargetInput is the DDC/CI VCP 0x60 value to switch the monitor to
	// when the trigger fires (i.e. the input of the *other* computer).
	// Common values: 0x0F=DisplayPort-1, 0x10=DisplayPort-2, 0x11=HDMI-1,
	// 0x12=HDMI-2, 0x1B=USB-C. These vary per monitor; the app can list what
	// the panel actually reports.
	MonitorTargetInput uint16 `json:"monitorTargetInput"`

	// MonitorMatch optionally restricts which physical display we drive, matched
	// against the model/serial reported over DDC. Empty = first DDC-capable one.
	MonitorMatch string `json:"monitorMatch"`

	// PushMouse, when true, sends the mouse a HID++ CHANGE HOST command on
	// trigger so it follows the keyboard (needed for number-key mode).
	PushMouse bool `json:"pushMouse"`

	// MouseTargetHost is the 0-based host slot to switch the mouse to when
	// PushMouse is set (host 1 on the device == 0 here).
	MouseTargetHost int `json:"mouseTargetHost"`

	// DebounceMs is how long a roam-away must persist before we act, to reject
	// momentary link blips. Default 400ms.
	DebounceMs int `json:"debounceMs"`

	// path is where this config was loaded from / will be saved to.
	path string
	mu   sync.Mutex
}

// Default returns a config with sensible starting values (disabled until the
// user picks a target input, so we never switch unexpectedly on first run).
func Default() *Config {
	return &Config{
		Enabled:            false,
		Trigger:            TriggerKeyboard,
		MonitorTargetInput: 0x0F, // DisplayPort-1; user should confirm
		PushMouse:          true,
		MouseTargetHost:    1, // host 2 on the device
		DebounceMs:         400,
	}
}

// DefaultPath returns the standard config file location for this OS.
func DefaultPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "logiMonitorSwitch", "config.json"), nil
}

// Load reads config from path, creating a default file if none exists.
func Load(path string) (*Config, error) {
	c := Default()
	c.path = path

	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return c, c.Save() // materialize a default file
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, c); err != nil {
		return nil, err
	}
	c.path = path
	if c.DebounceMs <= 0 {
		c.DebounceMs = 400
	}
	if c.Trigger == "" {
		c.Trigger = TriggerKeyboard
	}
	return c, nil
}

// Save atomically writes the config back to disk.
func (c *Config) Save() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if err := os.MkdirAll(filepath.Dir(c.path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, c.path)
}

// Path returns the file backing this config.
func (c *Config) Path() string { return c.path }
