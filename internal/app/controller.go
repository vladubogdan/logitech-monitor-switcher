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
	"logimonitorswitch/internal/powermon"
)

// wakeGrace is how long after resume we keep switching suspended, so the
// receiver's HID links have time to re-establish without looking like a roam.
const wakeGrace = 8 * time.Second

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

	// Power-transition suppression. While asleep, or until suppressUntil after
	// a wake, we never fire a switch and never re-arm — the HID link churn
	// around sleep/wake otherwise looks exactly like the device roaming away.
	asleep        bool
	suppressUntil time.Time

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
	powermon.Start()
	go c.watchPower()
	go c.supervise()
}

// watchPower suspends switching across sleep/wake transitions. On sleep we
// disarm; on wake we disarm and hold off for wakeGrace while the receiver's
// links re-establish, so the reconnection churn isn't mistaken for a roam.
func (c *Controller) watchPower() {
	for {
		select {
		case <-c.stop:
			return
		case ev := <-powermon.Events():
			switch ev {
			case powermon.Sleep:
				c.mu.Lock()
				c.asleep = true
				c.armed = false
				c.mu.Unlock()
				log.Printf("system sleeping — auto-switch suspended")
			case powermon.Wake:
				c.mu.Lock()
				c.asleep = false
				c.armed = false
				c.present = false
				c.suppressUntil = time.Now().Add(wakeGrace)
				c.mu.Unlock()
				log.Printf("system woke — auto-switch suspended for %s while links re-establish", wakeGrace)
			}
		}
	}
}

// isSuppressedLocked reports whether a power transition currently forbids
// firing/arming. Caller must hold c.mu.
//
// It consults powermon.Suppressed directly (not just c.asleep/c.suppressUntil,
// which are set by the watchPower goroutine) so the decision cannot be out-raced
// by that goroutine being scheduled late on wake — the exact window in which the
// reconnect churn used to slip through and fire a bogus switch.
func (c *Controller) isSuppressedLocked() bool {
	return c.asleep || time.Now().Before(c.suppressUntil) || powermon.Suppressed(wakeGrace)
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
	// so launching the app while the device is already away never fires a
	// switch — and never arm during a sleep/wake suppression window, so a
	// receiver that reconnects on wake doesn't arm mid-churn.
	present := trigger != 0 && c.pingPresent(conn, trigger)
	c.mu.Lock()
	c.present = present
	c.armed = present && !c.isSuppressedLocked()
	c.mu.Unlock()
	c.ui.SetPresent(present)
	if trigger != 0 {
		c.ui.SetStatus(c.statusLine(present))
	}

	poll := time.NewTicker(c.pollInterval)
	defer poll.Stop()

	// Debounce timer for a pending departure. The bool carried on pendingC records
	// whether the departure came from an authoritative disconnect notification
	// (true) or from the less-reliable poll fallback (false); it selects how hard
	// we re-confirm absence before firing.
	var pending *time.Timer
	pendingC := make(chan bool, 1)
	cancelPending := func() {
		if pending != nil {
			pending.Stop()
			pending = nil
		}
	}
	scheduleDepart := func(viaNote bool) {
		cancelPending()
		pending = time.AfterFunc(c.debounce(), func() { pendingC <- viaNote })
	}

	notes := conn.Notifications()
	lastPoll := present // for logging poll-detected presence transitions
	for {
		select {
		case <-c.stop:
			return nil

		case n, ok := <-notes:
			if !ok {
				return fmt.Errorf("receiver disconnected")
			}
			trig := c.triggerDevice()
			// Log every notification so a Windows log shows whether the receiver
			// is delivering device-connection events to us at all (the macOS vs.
			// Windows difference is in how these reports are read).
			log.Printf("hid notification: dev=%d subid=0x%02X params=% X (trigger=%d)", n.Device, n.SubID, n.Params, trig)
			if trig == 0 {
				continue
			}
			if chg, nowPresent := interpret(n, trig); chg {
				log.Printf("trigger %s (dev %d) presence via notification → present=%v", c.cfg.Trigger, trig, nowPresent)
				if nowPresent {
					cancelPending()
					c.onPresent()
				} else if c.onDepartStart() {
					log.Printf("departure detected via notification — debouncing %s", c.debounce())
					scheduleDepart(true) // authoritative
				}
			}

		case <-poll.C:
			trig := c.triggerDevice()
			if trig == 0 {
				continue
			}
			nowPresent := c.pingPresent(conn, trig)
			if nowPresent != lastPoll {
				log.Printf("poll: %s (dev %d) present=%v", c.cfg.Trigger, trig, nowPresent)
				lastPoll = nowPresent
			}
			if nowPresent {
				cancelPending()
				c.onPresent()
			} else if c.onDepartStart() {
				log.Printf("departure detected via poll — debouncing %s", c.debounce())
				scheduleDepart(false) // poll-based; confirm harder
			}

		case viaNote := <-pendingC:
			// Debounce elapsed; confirm still absent before acting. A disconnect
			// notification is authoritative, so one quick ping is enough. A
			// poll-detected departure is less reliable (a device streaming input
			// can delay its own ping reply), so re-check harder to avoid a false
			// switch while the device is really still here.
			trig := c.triggerDevice()
			stillHere := trig == 0
			if trig != 0 {
				if viaNote {
					stillHere = c.pingPresentQuick(conn, trig)
				} else {
					stillHere = c.pingPresentConfirm(conn, trig)
				}
			}
			if stillHere {
				log.Printf("debounce elapsed but %s still present — not switching", c.cfg.Trigger)
				c.onPresent()
				continue
			}
			log.Printf("debounce elapsed and %s confirmed absent — firing switch", c.cfg.Trigger)
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

// onPresent marks the device present again and re-arms for the next departure
// (unless a sleep/wake suppression window is active, in which case arming waits
// until the window clears).
func (c *Controller) onPresent() {
	c.mu.Lock()
	was := c.present
	c.present = true
	if !c.isSuppressedLocked() {
		c.armed = true
	}
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
	if c.isSuppressedLocked() {
		log.Printf("departure ignored: power-transition suppression active (asleep or just woke)")
		return false
	}
	return true
}

func (c *Controller) fireSwitch(conn *hidpp.Conn) {
	c.mu.Lock()
	if armed, enabled, suppressed := c.armed, c.cfg.Enabled, c.isSuppressedLocked(); !armed || !enabled || suppressed {
		c.mu.Unlock()
		log.Printf("switch aborted at fire time (armed=%v enabled=%v suppressed=%v)", armed, enabled, suppressed)
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

	// Push the mouse and switch the monitor concurrently: they hit different
	// hardware (the receiver vs. the display's DDC channel) and neither depends
	// on the other, so running them in parallel shaves the mouse-push time off
	// the user-visible switch latency.
	//
	// 1) Push the mouse to follow the keyboard (number-key mode). Harmless if
	//    the mouse already left (Flow mode).
	var pushWG sync.WaitGroup
	if pushMouse && mouseDev != 0 {
		pushWG.Add(1)
		go func() {
			defer pushWG.Done()
			if err := conn.SetHost(mouseDev, mouseHost); err != nil {
				log.Printf("push mouse to host %d: %v", mouseHost, err)
			} else {
				log.Printf("pushed mouse to host %d", mouseHost)
			}
		}()
	}

	// 2) Switch the monitor input. This must happen while we are still the
	//    active source — which we are, at the instant of departure.
	err := ddc.SetInputByMatch(match, input)
	pushWG.Wait()
	if err != nil {
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

// pingPresentQuick does a single short-timeout ping. Used to confirm an
// authoritative disconnect notification: a device that is really still here
// answers within a few ms, and one that has roamed away costs only the short
// timeout instead of a full confirm sweep.
func (c *Controller) pingPresentQuick(conn *hidpp.Conn, dev byte) bool {
	return conn.PingWithTimeout(dev, 150*time.Millisecond)
}

// pingPresentConfirm returns true if any of a few ping attempts succeeds. Used
// on the poll fallback path (no authoritative notification) to reject transient
// reply congestion before firing a switch.
func (c *Controller) pingPresentConfirm(conn *hidpp.Conn, dev byte) bool {
	for i := 0; i < 3; i++ {
		if conn.PingWithTimeout(dev, 250*time.Millisecond) {
			return true
		}
		time.Sleep(80 * time.Millisecond)
	}
	return false
}

func (c *Controller) debounce() time.Duration {
	c.mu.Lock()
	d := c.cfg.DebounceMs
	c.mu.Unlock()
	if d <= 0 {
		d = 250
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
