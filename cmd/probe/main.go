// Command probe is a temporary diagnostic for verifying DDC + HID++ on this
// machine. It is not part of the shipped app.
package main

import (
	"fmt"
	"os"
	"time"

	"logimonitorswitch/internal/autostart"
	"logimonitorswitch/internal/ddc"
	"logimonitorswitch/internal/hidpp"
	"logimonitorswitch/internal/hidtransport"
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "ddc":
			probeDDC()
			return
		case "autostart":
			probeAutostart()
			return
		case "ddcraw":
			probeDDCRaw()
			return
		}
	}
	probeHIDPP()
}

func probeAutostart() {
	mode := "status"
	if len(os.Args) > 2 {
		mode = os.Args[2]
	}
	switch mode {
	case "on":
		fmt.Println("Enable:", autostart.Enable())
	case "off":
		fmt.Println("Disable:", autostart.Disable())
	}
	fmt.Println("IsEnabled:", autostart.IsEnabled())
}

func probeDDC() {
	fmt.Println("== DDC displays ==")
	displays, _ := ddc.List()
	for _, d := range displays {
		cur, max, gerr := d.GetInput()
		if gerr != nil {
			fmt.Printf("  [%d] %-24s input read failed\n", d.Index, d.Name)
		} else {
			fmt.Printf("  [%d] %-24s input=0x%02X (%s) max=0x%02X\n",
				d.Index, d.Name, cur, ddc.InputSourceName(cur), max)
		}
		if caps, err := d.Capabilities(); err == nil {
			fmt.Printf("      caps: %.200s\n", caps)
		} else {
			fmt.Println("      caps read failed:", err)
		}
		// Safe reversible write test: dip brightness then restore.
		if b, bmax, err := d.GetVCP(0x10); err == nil {
			fmt.Printf("      brightness=%d/%d — dipping to test DDC write...\n", b, bmax)
			target := uint16(0)
			if b > 20 {
				target = b - 20
			}
			if err := d.SetVCP(0x10, target); err != nil {
				fmt.Println("      write failed:", err)
			} else {
				time.Sleep(900 * time.Millisecond)
				_ = d.SetVCP(0x10, b) // restore
				fmt.Println("      write OK (brightness dipped and restored)")
			}
		} else {
			fmt.Println("      brightness read failed:", err)
		}
		d.Close()
	}
}

func typeName(t byte) string {
	switch t {
	case hidpp.DeviceKeyboard:
		return "keyboard"
	case hidpp.DeviceMouse:
		return "mouse"
	default:
		return fmt.Sprintf("type-%d", t)
	}
}

func probeHIDPP() {
	rcv, err := hidtransport.OpenFirst()
	if err != nil {
		fmt.Println("Open error:", err)
		return
	}
	defer rcv.Close()
	fmt.Printf("Receiver PID=0x%04X path=%s\n", rcv.Info.ProductID, rcv.Info.Path)

	c := hidpp.Open(rcv)
	defer c.Close()

	if err := c.EnableNotifications(); err != nil {
		fmt.Println("enable notifications:", err)
	}

	fmt.Println("\n== Devices ==")
	for dev := byte(1); dev <= 6; dev++ {
		connected, maj, min := c.Ping(dev)
		if !connected {
			continue
		}
		line := fmt.Sprintf("  dev %d: connected HID++ %d.%d", dev, maj, min)
		if t, err := c.DeviceType(dev); err == nil {
			line += fmt.Sprintf("  type=%s", typeName(t))
		}
		if nb, cur, err := c.HostInfo(dev); err == nil {
			line += fmt.Sprintf("  CHANGE_HOST ok (hosts=%d current=%d)", nb, cur)
		} else {
			line += "  CHANGE_HOST unsupported"
		}
		fmt.Println(line)
	}

	// Consume the notification channel for a long window so we catch roam events.
	// Filter out routine mouse/keyboard input traffic (sub 0x0E, 0x00, etc.);
	// only connection-relevant notifications are interesting here.
	fmt.Println("\n>>> SWITCH NOW. Step 1: press the keyboard channel button to send")
	fmt.Println(">>>   the KEYBOARD to the other PC, wait ~4s, switch it BACK.")
	fmt.Println(">>> Step 2: Ctrl+drag to the edge (Flow) to send BOTH away, ~4s, back.")
	fmt.Println(">>> Watching 55s (only connection events shown)...")
	done := time.After(55 * time.Second)
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	start := time.Now()
	for {
		select {
		case n, ok := <-c.Notifications():
			if !ok {
				fmt.Println("(notification channel closed)")
				return
			}
			// Show only connection-related sub-ids.
			if n.SubID != 0x40 && n.SubID != 0x41 {
				continue
			}
			desc := ""
			if n.SubID == 0x41 && len(n.Params) >= 1 {
				if n.Params[0]&0x40 != 0 {
					desc = "  <-- LINK LOST (roamed away)"
				} else {
					desc = "  <-- link established (present)"
				}
			}
			if n.SubID == 0x40 {
				desc = "  <-- DISCONNECTION (0x40)"
			}
			fmt.Printf("  [%4.1fs] NOTIFY dev=%d sub=0x%02X params=% 02X%s\n",
				time.Since(start).Seconds(), n.Device, n.SubID, n.Params, desc)
		case <-tick.C:
			fmt.Printf("  [%4.1fs] ...listening (dev1 keyboard present=%v)\n",
				time.Since(start).Seconds(), func() bool { ok, _, _ := c.Ping(1); return ok }())
		case <-done:
			fmt.Println("== done ==")
			return
		}
	}
}
