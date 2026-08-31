//go:build darwin

package hidtransport

import (
	"log"

	hid "github.com/sstallion/go-hid"
)

// configureOpenMode makes hidapi open HID devices in shared (non-seize) mode.
//
// hidapi's macOS backend defaults to exclusive open (kIOHIDOptionsTypeSeizeDevice)
// for backward compatibility. Seizing the Logitech receiver prevents Logi
// Options+ from talking to it, which disables Flow (Ctrl+drag to the screen
// edge) while our app is running. Non-exclusive open lets both coexist.
func configureOpenMode() {
	hid.SetOpenExclusive(false)
	log.Printf("HID open mode: exclusive=%v (want false so Logi Options+/Flow keeps working)", hid.GetOpenExclusive())
}
