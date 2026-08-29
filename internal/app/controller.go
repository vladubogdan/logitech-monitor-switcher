// Package app is the orchestration layer: it identifies the keyboard/mouse on
// the receiver, watches for one of them roaming to another host, and on that
// event pushes the mouse to follow (if configured) and switches the monitor
// input over DDC.
package app

import (
	"fmt"
	"log"
	"sync"
	"time"

	"logimonitorswitch/internal/config"
	"logimonitorswitch/internal/ddc"
	"logimonitorswitch/internal/hidpp"
	"logimonitorswitch/internal/hidtransport"
)

// UI receives status updates so the tray can reflect state. All methods must be
// safe to call from a background goroutine.
type UI interface {
	SetStatus(text string)
	SetPresent(present bool)
	SetReceiver(connected bool, name string)
	Notify(title, body string)
}

// nopUI is used when no UI is attached.
type nopUI struct{}

func (nopUI) SetStatus(string)         {}
func (nopUI) SetPresent(bool)          {}
func (nopUI) SetReceiver(bool, string) {}
func (nopUI) Notify(string, string)    {}

// Controller runs the detect-and-switch loop.
type Controller struct {
	cfg *config.Config
	ui  UI

	mu          sync.Mutex
	keyboardDev byte
	mouseDev    byte
	present     bool // is the trigger device currently on this host?
	armed       bool // may we fire a switch on the next departure?

	pollInterval time.Duration

	stop chan struct{}
	done chan struct{}
}

// New creates a Controller.
func New(cfg *config.Config, ui UI) *Controller {
	if ui == nil {
		ui = nopUI{}
	}
	return &Controller{
		cfg:          cfg,
		ui:           ui,
		pollInterval: 2 * time.Second,
		stop:         make(chan struct{}),
		done:         make(chan struct{}),
	}
}

// Start runs the supervisor loop in the background. It keeps (re)opening the
// receiver if it goes away.
func (c *Controller) Start() {
	go c.supervise()
}

// Stop signals the loop to exit and waits for it.
func (c *Controller) Stop() {
	close(c.stop)
	<-c.done
}

func (c *Controller) supervise() {
	defer close(c.done)
	for {
		select {
		case <-c.stop:
			return
		default:
		}
		if err := c.runOnce(); err != nil {
			c.ui.SetReceiver(false, "")
			c.ui.SetStatus("Waiting for receiver: " + err.Error())
			log.Printf("receiver session ended: %v", err)
		}
		select {
		case <-c.stop:
			return
		case <-time.After(3 * time.Second): // retry backoff
		}
	}
}

// runOnce opens the receiver, identifies devices, and runs the detection loop
// until the receiver disconnects or Stop is called.
func (c *Controller) runOnce() error {
	rcv, err := hidtransport.OpenFirst()
	if err != nil {
		return err
	}
	conn := hidpp.Open(rcv)
	defer conn.Close()

	rcvName := fmt.Sprintf("0x%04X", rcv.Info.ProductID)
	log.Printf("receiver opened: %s", rcv.Info.Summary())
	c.ui.SetReceiver(true, rcvName)
	if err := conn.EnableNotifications(); err != nil {
		log.Printf("enable notifications: %v", err)
	}
	c.identifyDevices(conn)
	c.ui.SetReceiver(true, rcvName) // refresh the devices line now that we know them

	// Self-check the CHANGE HOST (long-reply) path and log it — this is the
	// exchange that fails on Windows if we don't read every HID++ collection.
	if md := c.MouseDev(); md != 0 {
		if nb, cur, err := conn.HostInfo(md); err != nil {
			log.Printf("mouse dev %d CHANGE HOST check FAILED: %v", md, err)
		} else {
			log.Printf("mouse dev %d CHANGE HOST ok: hosts=%d current=%d", md, nb, cur)
		}
	}

	trigger := c.triggerDevice()
	if trigger == 0 {
		c.ui.SetStatus("No " + string(c.cfg.Trigger) + " found on receiver")
	}

	// Initialise presence. Critically, only arm if the device is present *now*,
	// so launching the app while the device is already away never fires a switch.
	present := trigger != 0 && c.pingPresent(conn, trigger)
	c.setState(present, present)
	c.ui.SetPresent(present)
	if trigger != 0 {
		c.ui.SetStatus(c.statusLine(present))
	}

	poll := time.NewTicker(c.pollInterval)
	defer poll.Stop()

	// Debounce timer for a pending departure.
	var pending *time.Timer
	pendingC := make(chan struct{}, 1)
	cancelPending := func() {
		if pending != nil {
			pending.Stop()
			pending = nil
		}
	}

	notes := conn.Notifications()
	for {
		select {
		case <-c.stop:
			return nil

		case n, ok := <-notes:
			if !ok {
				return fmt.Errorf("receiver disconnected")
			}
			trig := c.triggerDevice()
			if trig == 0 {
				continue
			}
			if chg, nowPresent := interpret(n, trig); chg {
				if nowPresent {
					cancelPending()
					c.onPresent()
				} else if c.onDepartStart() {
					cancelPending()
					pending = time.AfterFunc(c.debounce(), func() { pendingC <- struct{}{} })
				}
			}

		case <-poll.C:
			trig := c.triggerDevice()
			if trig == 0 {
				continue
			}
			nowPresent := c.pingPresent(conn, trig)
			if nowPresent {
				cancelPending()
				c.onPresent()
			} else if c.onDepartStart() {
				cancelPending()
				pending = time.AfterFunc(c.debounce(), func() { pendingC <- struct{}{} })
			}

		case <-pendingC:
			// Debounce elapsed; confirm still absent before acting. Require
			// several ping attempts to all fail, so a single congested reply
			// (common while a device streams input) can't cause a false switch.
			trig := c.triggerDevice()
			if trig == 0 || c.pingPresentConfirm(conn, trig) {
				c.onPresent()
				continue
			}
			c.fireSwitch(conn)
		}
	}
}

// interpret decides whether a notification changes the trigger device's
// presence. Returns (changed, present).
func interpret(n hidpp.Notification, trigger byte) (bool, bool) {
	if n.Device != trigger {
		return false, false
	}
	switch n.SubID {
	case 0x41: // device-connection: bit 0x40 of params[0] == link NOT established
		if len(n.Params) >= 1 {
			return true, n.Params[0]&0x40 == 0
		}
	case 0x40: // device-disconnection
		return true, false
	}
	return false, false
}

// --- state helpers (guarded by mu) ---------------------------------------

func (c *Controller) setState(present, armed bool) {
	c.mu.Lock()
	c.present, c.armed = present, armed
	c.mu.Unlock()
}

// onPresent marks the device present again and re-arms for the next departure.
func (c *Controller) onPresent() {
	c.mu.Lock()
	was := c.present
	c.present, c.armed = true, true
	c.mu.Unlock()
	if !was {
		c.ui.SetPresent(true)
		c.ui.SetStatus(c.statusLine(true))
		log.Printf("%s returned to this host", c.cfg.Trigger)
	}
}

// onDepartStart records a departure and reports whether we should begin a
// (debounced) switch. Returns true only on the present→absent edge while armed
// and enabled.
func (c *Controller) onDepartStart() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.present {
		return false // already handling / already gone
	}
	c.present = false
	if !c.armed || !c.cfg.Enabled {
		return false
	}
	return true
}

func (c *Controller) fireSwitch(conn *hidpp.Conn) {
	c.mu.Lock()
	if !c.armed || !c.cfg.Enabled {
		c.mu.Unlock()
		return
	}
	c.armed = false // don't refire until the device returns
	mouseDev := c.mouseDev
	pushMouse := c.cfg.PushMouse
	mouseHost := byte(c.cfg.MouseTargetHost)
	input := c.cfg.MonitorTargetInput
	match := c.cfg.MonitorMatch
	c.mu.Unlock()

	c.ui.SetPresent(false)
	log.Printf("%s roamed away — performing switch", c.cfg.Trigger)

	// 1) Push the mouse to follow the keyboard (number-key mode). Harmless if
	//    the mouse already left (Flow mode).
	if pushMouse && mouseDev != 0 {
		if err := conn.SetHost(mouseDev, mouseHost); err != nil {
			log.Printf("push mouse to host %d: %v", mouseHost, err)
		} else {
			log.Printf("pushed mouse to host %d", mouseHost)
		}
	}

	// 2) Switch the monitor input. This must happen while we are still the
	//    active source — which we are, at the instant of departure.
	if err := ddc.SetInputByMatch(match, input); err != nil {
		log.Printf("monitor switch failed: %v", err)
		c.ui.SetStatus("Monitor switch FAILED: " + err.Error())
		c.ui.Notify("logiMonitorSwitch", "Monitor switch failed: "+err.Error())
		return
	}
	name := ddc.InputSourceName(input)
	log.Printf("monitor switched to 0x%02X (%s)", input, name)
	c.ui.SetStatus(fmt.Sprintf("Switched monitor → %s", name))
	c.ui.Notify("logiMonitorSwitch", fmt.Sprintf("Switched monitor to %s", name))
}

// --- device identification ------------------------------------------------

func (c *Controller) identifyDevices(conn *hidpp.Conn) {
	// Discover connected device indices from the receiver's enumerate
	// notifications, which are reliable even when a device is streaming input
	// (unlike active pings, whose replies get delayed behind that stream).
	connected := c.discoverConnected(conn)

	types := map[byte]byte{}
	for _, dev := range connected {
		// Retry device-type: a device in active use can drown its own reply.
		for attempt := 0; attempt < 4; attempt++ {
			if t, err := conn.DeviceType(dev); err == nil {
				types[dev] = t
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
	}

	var kbd, mouse byte
	// First assign from confirmed types.
	for _, dev := range connected {
		switch types[dev] {
		case hidpp.DeviceKeyboard:
			if kbd == 0 {
				kbd = dev
			}
		case hidpp.DeviceMouse:
			if mouse == 0 {
				mouse = dev
			}
		}
	}
	// Fallback: a connected device whose type we couldn't read (typically a
	// busy mouse) fills the still-empty mouse (then keyboard) slot.
	for _, dev := range connected {
		if dev == kbd || dev == mouse {
			continue
		}
		if _, typed := types[dev]; typed {
			continue // typed as something we don't use
		}
		if mouse == 0 {
			mouse = dev
			log.Printf("device %d: type unknown (busy?) — assuming mouse", dev)
		} else if kbd == 0 {
			kbd = dev
			log.Printf("device %d: type unknown (busy?) — assuming keyboard", dev)
		}
	}

	c.mu.Lock()
	c.keyboardDev, c.mouseDev = kbd, mouse
	c.mu.Unlock()
	log.Printf("identified devices: keyboard=%d mouse=%d (connected=%v)", kbd, mouse, connected)
}

// discoverConnected asks the receiver to re-announce its paired devices and
// collects the indices that report an established link. Falls back to a ping
// scan if no notifications arrive.
func (c *Controller) discoverConnected(conn *hidpp.Conn) []byte {
	present := map[byte]bool{}
	_ = conn.Enumerate()
	deadline := time.After(1200 * time.Millisecond)
collect:
	for {
		select {
		case n, ok := <-conn.Notifications():
			if !ok {
				break collect
			}
			if n.SubID == 0x41 && len(n.Params) >= 1 {
				present[n.Device] = n.Params[0]&0x40 == 0
			}
		case <-deadline:
			break collect
		}
	}
	var connected []byte
	for dev := byte(1); dev <= 6; dev++ {
		if present[dev] {
			connected = append(connected, dev)
		}
	}
	if len(connected) == 0 {
		// Fallback: direct ping scan (works when the bus is quiet).
		for dev := byte(1); dev <= 6; dev++ {
			if ok, _, _ := conn.Ping(dev); ok {
				connected = append(connected, dev)
			}
		}
	}
	return connected
}

func (c *Controller) triggerDevice() byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch c.cfg.Trigger {
	case config.TriggerMouse:
		return c.mouseDev
	case config.TriggerEither:
		if c.keyboardDev != 0 {
			return c.keyboardDev
		}
		return c.mouseDev
	default: // keyboard
		return c.keyboardDev
	}
}

func (c *Controller) pingPresent(conn *hidpp.Conn, dev byte) bool {
	ok, _, _ := conn.Ping(dev)
	return ok
}

// pingPresentConfirm returns true if any of a few ping attempts succeeds. Used
// before firing a switch to reject transient reply congestion.
func (c *Controller) pingPresentConfirm(conn *hidpp.Conn, dev byte) bool {
	for i := 0; i < 3; i++ {
		if ok, _, _ := conn.Ping(dev); ok {
			return true
		}
		time.Sleep(120 * time.Millisecond)
	}
	return false
}

func (c *Controller) debounce() time.Duration {
	c.mu.Lock()
	d := c.cfg.DebounceMs
	c.mu.Unlock()
	if d <= 0 {
		d = 400
	}
	return time.Duration(d) * time.Millisecond
}

func (c *Controller) statusLine(present bool) string {
	if !c.cfg.Enabled {
		return "Disabled"
	}
	if present {
		return fmt.Sprintf("Armed · %s present", c.cfg.Trigger)
	}
	return fmt.Sprintf("%s away", c.cfg.Trigger)
}

// KeyboardDev / MouseDev expose the identified indices (for the tray).
func (c *Controller) KeyboardDev() byte { c.mu.Lock(); defer c.mu.Unlock(); return c.keyboardDev }
func (c *Controller) MouseDev() byte    { c.mu.Lock(); defer c.mu.Unlock(); return c.mouseDev }

// --- setters used by the tray (mutate config under lock, then persist) ----

func (c *Controller) save() {
	if err := c.cfg.Save(); err != nil {
		log.Printf("save config: %v", err)
	}
}

// SetEnabled toggles automatic switching. Re-arming only happens when the
// device is (re)confirmed present, so enabling while the device is away will
// not immediately fire.
func (c *Controller) SetEnabled(v bool) {
	c.mu.Lock()
	c.cfg.Enabled = v
	if !v {
		c.armed = false
	} else if c.present {
		c.armed = true
	}
	present := c.present
	c.mu.Unlock()
	c.save()
	c.ui.SetStatus(c.statusLine(present))
}

func (c *Controller) SetTrigger(t config.TriggerDevice) {
	c.mu.Lock()
	c.cfg.Trigger = t
	c.mu.Unlock()
	c.save()
}

func (c *Controller) SetTargetInput(v uint16) {
	c.mu.Lock()
	c.cfg.MonitorTargetInput = v
	c.mu.Unlock()
	c.save()
}

func (c *Controller) SetMonitorMatch(m string) {
	c.mu.Lock()
	c.cfg.MonitorMatch = m
	c.mu.Unlock()
	c.save()
}

func (c *Controller) SetPushMouse(v bool) {
	c.mu.Lock()
	c.cfg.PushMouse = v
	c.mu.Unlock()
	c.save()
}

func (c *Controller) SetMouseHost(hostIdx int) {
	c.mu.Lock()
	c.cfg.MouseTargetHost = hostIdx
	c.mu.Unlock()
	c.save()
}

// Config returns a snapshot copy of the current settings (value fields only).
func (c *Controller) Config() config.Config {
	c.mu.Lock()
	defer c.mu.Unlock()
	return config.Config{
		Enabled:            c.cfg.Enabled,
		Trigger:            c.cfg.Trigger,
		MonitorTargetInput: c.cfg.MonitorTargetInput,
		MonitorMatch:       c.cfg.MonitorMatch,
		PushMouse:          c.cfg.PushMouse,
		MouseTargetHost:    c.cfg.MouseTargetHost,
		DebounceMs:         c.cfg.DebounceMs,
	}
}

// TestSwitchMonitor performs the monitor input switch immediately, ignoring the
// state machine. Used by the tray "test" action so the user can verify DDC
// works and find the right input code — safely and on demand.
func (c *Controller) TestSwitchMonitor(input uint16) error {
	c.mu.Lock()
	match := c.cfg.MonitorMatch
	c.mu.Unlock()
	return ddc.SetInputByMatch(match, input)
}
