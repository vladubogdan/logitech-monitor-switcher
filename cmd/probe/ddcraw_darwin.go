//go:build darwin

package main

import (
	"fmt"

	"logimonitorswitch/internal/ddc"
)

// probeDDCRaw tries several read offsets/sizes to find what actually returns a
// valid DDC reply on this Mac + monitor.
func probeDDCRaw() {
	displays, _ := ddc.List()
	if len(displays) == 0 {
		fmt.Println("no displays")
		return
	}
	d := displays[0]
	defer d.Close()
	fmt.Printf("Display: %s\n", d.Name)
	offsets := []uint32{0x51, 0x00, 0x6E, 0x6F}
	for _, off := range offsets {
		fmt.Printf("--- read offset 0x%02X ---\n", off)
		for attempt := 0; attempt < 6; attempt++ {
			buf, ret := d.DebugRead(off, 12)
			fmt.Printf("  ret=0x%08X  % 02X\n", ret, buf)
		}
	}
}
