// Package ddc switches monitor inputs over DDC/CI (VCP feature 0x60).
//
// The concrete Display type and List() live in the per-OS files:
//
//	ddc_darwin.go  — Apple Silicon via the private IOAVService I2C API (m1ddc-style)
//	ddc_windows.go — via the high-level monitor API in dxva2.dll
//
// IMPORTANT: DDC/CI only reaches a monitor over the link that is currently
// displaying. A computer can therefore only switch the panel *away* to another
// input while it is the active source; it cannot switch a panel that is showing
// someone else. That is by design — the peer computer switches it back.
package ddc

import (
	"fmt"
	"strconv"
	"strings"
)

// SupportedInputs queries the monitor's capabilities and returns the input
// source (VCP 0x60) codes it actually supports. Returns an error when the
// monitor doesn't answer capability reads (common on Apple Silicon / some
// panels) — callers should fall back to the predefined list.
func SupportedInputs(match string) ([]uint16, error) {
	displays, err := List()
	if err != nil {
		return nil, err
	}
	defer func() {
		for _, d := range displays {
			d.Close()
		}
	}()
	for _, d := range displays {
		if match != "" && !strings.Contains(strings.ToLower(d.Name), strings.ToLower(match)) {
			continue
		}
		caps, err := d.Capabilities()
		if err != nil {
			return nil, err
		}
		inputs := parseInputsFromCaps(caps)
		if len(inputs) == 0 {
			return nil, fmt.Errorf("no input list in capabilities")
		}
		return inputs, nil
	}
	return nil, fmt.Errorf("no matching display")
}

// InputSourceNames maps VCP 0x60 input-source values to human labels. The
// 0x01–0x12 block is the standard MCCS table; the higher codes are common
// vendor USB-C / Thunderbolt values (which vary per monitor). This is the
// fallback list used when the monitor won't report its own capabilities;
// actual accepted values vary, so treat unknown-monitor labels as hints.
var InputSourceNames = map[uint16]string{
	0x01: "VGA-1",
	0x02: "VGA-2",
	0x03: "DVI-1",
	0x04: "DVI-2",
	0x05: "Composite-1",
	0x06: "Composite-2",
	0x07: "S-Video-1",
	0x08: "S-Video-2",
	0x0B: "Component-1",
	0x0C: "Component-2",
	0x0F: "DisplayPort-1",
	0x10: "DisplayPort-2",
	0x11: "HDMI-1",
	0x12: "HDMI-2",
	0x19: "USB-C",
	0x1B: "USB-C / DP-Alt",
	0x1C: "USB-C-2",
	0x1D: "USB-C-3",
	0x20: "USB-C / TB",
	0x21: "USB-C / TB-2",
	0x27: "USB-C (vendor)",
}

// InputSourceName returns a friendly label for a VCP 0x60 value.
func InputSourceName(v uint16) string {
	if n, ok := InputSourceNames[v]; ok {
		return n
	}
	return "Input"
}

// parseInputsFromCaps extracts the VCP 0x60 (input source) value list from a
// DDC/MCCS capabilities string, e.g. "...vcp(10 12 60(0F 11 12) D6(01 04)...)".
// Returns the list of supported input codes, or nil if not present.
func parseInputsFromCaps(caps string) []uint16 {
	c := strings.ToLower(caps)
	c = strings.ReplaceAll(c, " (", "(") // normalise "60 (" -> "60("
	if i := strings.Index(c, "vcp("); i >= 0 {
		c = c[i:] // restrict to the vcp(...) section
	}
	// Find "60(" where the preceding char is a token boundary, so we don't
	// match a value token inside another code's list.
	from := 0
	for {
		idx := strings.Index(c[from:], "60(")
		if idx < 0 {
			return nil
		}
		abs := from + idx
		if abs == 0 || c[abs-1] == ' ' || c[abs-1] == '(' {
			rest := c[abs+3:]
			end := strings.IndexByte(rest, ')')
			if end < 0 {
				return nil
			}
			var out []uint16
			for _, f := range strings.Fields(rest[:end]) {
				if v, err := strconv.ParseUint(f, 16, 16); err == nil {
					out = append(out, uint16(v))
				}
			}
			return out
		}
		from = abs + 3
	}
}
