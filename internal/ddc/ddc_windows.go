//go:build windows

package ddc

import (
	"fmt"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	user32                = windows.NewLazySystemDLL("user32.dll")
	dxva2                 = windows.NewLazySystemDLL("dxva2.dll")
	procEnumDisplayMons   = user32.NewProc("EnumDisplayMonitors")
	procGetNumPhysMon     = dxva2.NewProc("GetNumberOfPhysicalMonitorsFromHMONITOR")
	procGetPhysMon        = dxva2.NewProc("GetPhysicalMonitorsFromHMONITOR")
	procDestroyPhysMon    = dxva2.NewProc("DestroyPhysicalMonitor")
	procSetVCPFeature     = dxva2.NewProc("SetVCPFeature")
	procGetVCPFeatureRepl = dxva2.NewProc("GetVCPFeatureAndVCPFeatureReply")
	procGetCapsLen        = dxva2.NewProc("GetCapabilitiesStringLength")
	procGetCaps           = dxva2.NewProc("CapabilitiesRequestAndCapabilitiesReply")
)

// physicalMonitor mirrors the Win32 PHYSICAL_MONITOR struct.
type physicalMonitor struct {
	handle      windows.Handle
	description [128]uint16
}

// Display is one DDC-controllable monitor (a physical monitor handle).
type Display struct {
	Index  int
	Name   string
	handle windows.Handle
}

// List enumerates physical monitors and their handles.
func List() ([]*Display, error) {
	var hmonitors []windows.Handle
	cb := syscall.NewCallback(func(hMonitor windows.Handle, hdc uintptr, rect uintptr, lparam uintptr) uintptr {
		hmonitors = append(hmonitors, hMonitor)
		return 1 // continue
	})
	r, _, err := procEnumDisplayMons.Call(0, 0, cb, 0)
	if r == 0 {
		return nil, fmt.Errorf("EnumDisplayMonitors failed: %v", err)
	}

	var out []*Display
	idx := 0
	for _, hm := range hmonitors {
		var n uint32
		if r, _, _ := procGetNumPhysMon.Call(uintptr(hm), uintptr(unsafe.Pointer(&n))); r == 0 || n == 0 {
			continue
		}
		mons := make([]physicalMonitor, n)
		if r, _, _ := procGetPhysMon.Call(uintptr(hm), uintptr(n), uintptr(unsafe.Pointer(&mons[0]))); r == 0 {
			continue
		}
		for i := range mons {
			out = append(out, &Display{
				Index:  idx,
				Name:   strings.TrimRight(windows.UTF16ToString(mons[i].description[:]), "\x00"),
				handle: mons[i].handle,
			})
			idx++
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no physical monitors found")
	}
	return out, nil
}

// SetVCP writes an arbitrary VCP feature.
func (d *Display) SetVCP(vcp uint8, v uint16) error {
	r, _, err := procSetVCPFeature.Call(uintptr(d.handle), uintptr(vcp), uintptr(v))
	if r == 0 {
		return fmt.Errorf("SetVCPFeature 0x%02X=0x%04X failed: %v", vcp, v, err)
	}
	return nil
}

// GetVCP reads an arbitrary VCP feature.
func (d *Display) GetVCP(vcp uint8) (cur, max uint16, err error) {
	var vct uint32
	var curV, maxV uint32
	r, _, e := procGetVCPFeatureRepl.Call(
		uintptr(d.handle), uintptr(vcp),
		uintptr(unsafe.Pointer(&vct)),
		uintptr(unsafe.Pointer(&curV)),
		uintptr(unsafe.Pointer(&maxV)),
	)
	if r == 0 {
		return 0, 0, fmt.Errorf("GetVCPFeature 0x%02X failed: %v", vcp, e)
	}
	return uint16(curV), uint16(maxV), nil
}

// Capabilities returns the monitor's raw DDC/MCCS capabilities string.
func (d *Display) Capabilities() (string, error) {
	var length uint32
	if r, _, e := procGetCapsLen.Call(uintptr(d.handle), uintptr(unsafe.Pointer(&length))); r == 0 || length == 0 {
		return "", fmt.Errorf("GetCapabilitiesStringLength failed: %v", e)
	}
	buf := make([]byte, length)
	if r, _, e := procGetCaps.Call(uintptr(d.handle), uintptr(unsafe.Pointer(&buf[0])), uintptr(length)); r == 0 {
		return "", fmt.Errorf("CapabilitiesRequest failed: %v", e)
	}
	return string(buf[:length-1]), nil // strip trailing NUL
}

// SetInput switches the monitor's active input (VCP 0x60) to value v.
func (d *Display) SetInput(v uint16) error { return d.SetVCP(0x60, v) }

// GetInput reads the current input source (VCP 0x60).
func (d *Display) GetInput() (cur, max uint16, err error) { return d.GetVCP(0x60) }

// Close releases the physical monitor handle.
func (d *Display) Close() {
	if d.handle != 0 {
		procDestroyPhysMon.Call(uintptr(d.handle))
		d.handle = 0
	}
}

// SetInputByMatch switches the first display whose name contains match
// (case-insensitive). Empty match uses the first display.
func SetInputByMatch(match string, v uint16) error {
	displays, err := List()
	if err != nil {
		return err
	}
	defer func() {
		for _, d := range displays {
			d.Close()
		}
	}()
	for _, d := range displays {
		if match == "" || strings.Contains(strings.ToLower(d.Name), strings.ToLower(match)) {
			return d.SetInput(v)
		}
	}
	return fmt.Errorf("no display matched %q", match)
}
