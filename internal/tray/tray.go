// Package tray builds the menu-bar / system-tray UI and wires its controls to
// the app controller. It implements app.UI so the controller can push status
// updates back into the menu.
package tray

import (
	"fmt"
	"log"
	"os/exec"
	"runtime"
	"sort"

	"fyne.io/systray"

	"logimonitorswitch/internal/app"
	"logimonitorswitch/internal/autostart"
	"logimonitorswitch/internal/config"
	"logimonitorswitch/internal/ddc"
)

type inputOption struct {
	code  uint16
	label string
}

// Tray owns the menu and the controller.
type Tray struct {
	cfg  *config.Config
	ctrl *app.Controller

	inputs []inputOption

	// menu items (created in onReady)
	mStatus   *systray.MenuItem
	mReceiver *systray.MenuItem
	mDevices  *systray.MenuItem
	mEnabled  *systray.MenuItem

	mTrigItems map[config.TriggerDevice]*systray.MenuItem
	mInputItem map[uint16]*systray.MenuItem
	mTestItem  map[uint16]*systray.MenuItem
	mPush      *systray.MenuItem
	mHostItems map[int]*systray.MenuItem

	present bool
}

// Run starts the tray event loop (blocks until Quit).
func Run(cfg *config.Config) {
	t := &Tray{cfg: cfg}
	systray.Run(t.onReady, t.onExit)
}

func inputLabel(code uint16) string {
	return fmt.Sprintf("%s (0x%02X)", ddc.InputSourceName(code), code)
}

// computeInputs populates t.inputs, preferring the monitor's real capability
// list and falling back to the predefined table. Returns whether the list came
// from the monitor.
func (t *Tray) computeInputs() bool {
	detected := false
	if codes, err := ddc.SupportedInputs(t.cfg.MonitorMatch); err == nil && len(codes) > 0 {
		t.inputs = nil
		for _, code := range codes {
			t.inputs = append(t.inputs, inputOption{code, inputLabel(code)})
		}
		detected = true
		log.Printf("monitor inputs detected from capabilities: %v", codes)
	} else {
		if err != nil {
			log.Printf("monitor input detection unavailable (%v); using default list", err)
		}
		for code := range ddc.InputSourceNames {
			t.inputs = append(t.inputs, inputOption{code, inputLabel(code)})
		}
	}
	// Always include the currently-configured target so it can be shown/selected.
	t.ensureInput(t.cfg.MonitorTargetInput)
	sort.Slice(t.inputs, func(i, j int) bool { return t.inputs[i].code < t.inputs[j].code })
	return detected
}

func (t *Tray) ensureInput(code uint16) {
	for _, o := range t.inputs {
		if o.code == code {
			return
		}
	}
	t.inputs = append(t.inputs, inputOption{code, inputLabel(code)})
}

func (t *Tray) onReady() {
	t.applyIcon(true)
	systray.SetTitle("")
	systray.SetTooltip("logiMonitorSwitch")

	title := systray.AddMenuItem("logiMonitorSwitch", "")
	title.Disable()
	t.mStatus = systray.AddMenuItem("Status: starting…", "")
	t.mStatus.Disable()
	t.mReceiver = systray.AddMenuItem("Receiver: —", "")
	t.mReceiver.Disable()
	t.mDevices = systray.AddMenuItem("Devices: —", "")
	t.mDevices.Disable()

	systray.AddSeparator()

	t.mEnabled = systray.AddMenuItemCheckbox("Enabled (auto-switch)", "Turn automatic switching on/off", t.cfg.Enabled)
	go t.loop(t.mEnabled.ClickedCh, func() {
		on := !t.ctrl.Config().Enabled
		t.ctrl.SetEnabled(on)
		setCheck(t.mEnabled, on)
	})

	// Trigger submenu.
	trig := systray.AddMenuItem("Trigger on", "Which device leaving fires a switch")
	t.mTrigItems = map[config.TriggerDevice]*systray.MenuItem{}
	for _, td := range []config.TriggerDevice{config.TriggerKeyboard, config.TriggerMouse, config.TriggerEither} {
		it := trig.AddSubMenuItemCheckbox(string(td), "", t.cfg.Trigger == td)
		t.mTrigItems[td] = it
		td := td
		go t.loop(it.ClickedCh, func() {
			t.ctrl.SetTrigger(td)
			t.refreshTrigger()
		})
	}

	// Populate the input list from the monitor's real capabilities where
	// possible (falls back to the predefined table).
	detected := t.computeInputs()
	inpTitle := "Switch monitor to (on roam) — default list"
	if detected {
		inpTitle = "Switch monitor to (on roam) — from monitor"
	}

	// Target input submenu (the input to switch to on roam).
	inp := systray.AddMenuItem(inpTitle, "Input the monitor changes to when the device leaves")
	t.mInputItem = map[uint16]*systray.MenuItem{}
	for _, opt := range t.inputs {
		it := inp.AddSubMenuItemCheckbox(opt.label, "", opt.code == t.cfg.MonitorTargetInput)
		t.mInputItem[opt.code] = it
		code := opt.code
		go t.loop(it.ClickedCh, func() {
			t.ctrl.SetTargetInput(code)
			t.refreshInputs()
		})
	}

	// Push-mouse checkbox + host submenu.
	t.mPush = systray.AddMenuItemCheckbox("Push mouse to follow (number-key mode)", "Send the mouse a CHANGE HOST command on switch", t.cfg.PushMouse)
	go t.loop(t.mPush.ClickedCh, func() {
		on := !t.ctrl.Config().PushMouse
		t.ctrl.SetPushMouse(on)
		setCheck(t.mPush, on)
	})
	host := systray.AddMenuItem("Mouse target host", "Which host slot the mouse switches to")
	t.mHostItems = map[int]*systray.MenuItem{}
	for i := 0; i < 3; i++ {
		it := host.AddSubMenuItemCheckbox(fmt.Sprintf("Host %d", i+1), "", t.cfg.MouseTargetHost == i)
		t.mHostItems[i] = it
		idx := i
		go t.loop(it.ClickedCh, func() {
			t.ctrl.SetMouseHost(idx)
			t.refreshHosts()
		})
	}

	systray.AddSeparator()

	// Test submenu: switch the monitor now to a chosen input (verify DDC).
	test := systray.AddMenuItem("Test: switch monitor now", "Immediately switch the monitor (to find the right input)")
	t.mTestItem = map[uint16]*systray.MenuItem{}
	for _, opt := range t.inputs {
		it := test.AddSubMenuItem(opt.label, "")
		code := opt.code
		go t.loop(it.ClickedCh, func() {
			if err := t.ctrl.TestSwitchMonitor(code); err != nil {
				t.SetStatus("Test switch failed: " + err.Error())
			} else {
				t.SetStatus(fmt.Sprintf("Test: switched to %s", ddc.InputSourceName(code)))
			}
		})
	}

	startup := systray.AddMenuItemCheckbox("Start at login", "Launch automatically when you log in", autostart.IsEnabled())
	go t.loop(startup.ClickedCh, func() {
		on := !autostart.IsEnabled()
		var err error
		if on {
			err = autostart.Enable()
		} else {
			err = autostart.Disable()
		}
		if err != nil {
			t.SetStatus("Start at login failed: " + err.Error())
		}
		setCheck(startup, autostart.IsEnabled())
	})

	openCfg := systray.AddMenuItem("Open config file…", "")
	go t.loop(openCfg.ClickedCh, func() { openPath(t.cfg.Path()) })

	systray.AddSeparator()
	quit := systray.AddMenuItem("Quit", "")
	go t.loop(quit.ClickedCh, func() { systray.Quit() })

	// Start the controller now that the menu exists.
	t.ctrl = app.New(t.cfg, t)
	t.ctrl.Start()
}

func (t *Tray) onExit() {
	if t.ctrl != nil {
		t.ctrl.Stop()
	}
}

// loop invokes fn every time the channel signals (until closed).
func (t *Tray) loop(ch <-chan struct{}, fn func()) {
	for range ch {
		fn()
	}
}

// setCheck forces a checkbox item to a specific state.
func setCheck(it *systray.MenuItem, on bool) {
	if on {
		it.Check()
	} else {
		it.Uncheck()
	}
}

func (t *Tray) applyIcon(present bool) {
	png := iconPresentPNG()
	if !present {
		png = iconAwayPNG()
	}
	if runtime.GOOS == "windows" {
		systray.SetIcon(icoBytes(png))
	} else {
		systray.SetTemplateIcon(png, png)
	}
}

// --- app.UI implementation -----------------------------------------------

func (t *Tray) SetStatus(text string) {
	if t.mStatus != nil {
		t.mStatus.SetTitle("Status: " + text)
	}
}

func (t *Tray) SetPresent(present bool) {
	t.present = present
	t.applyIcon(present)
}

func (t *Tray) SetReceiver(connected bool, name string) {
	if t.mReceiver == nil {
		return
	}
	if connected {
		t.mReceiver.SetTitle("Receiver: connected " + name)
		if t.mDevices != nil && t.ctrl != nil {
			t.mDevices.SetTitle(fmt.Sprintf("Devices: keyboard=%d mouse=%d", t.ctrl.KeyboardDev(), t.ctrl.MouseDev()))
		}
	} else {
		t.mReceiver.SetTitle("Receiver: not found")
	}
}

func (t *Tray) Notify(title, body string) { notify(title, body) }

// --- refreshers keep the checkbox groups exclusive -----------------------

func (t *Tray) refreshTrigger() {
	cur := t.ctrl.Config().Trigger
	for td, it := range t.mTrigItems {
		if td == cur {
			it.Check()
		} else {
			it.Uncheck()
		}
	}
}

func (t *Tray) refreshInputs() {
	cur := t.ctrl.Config().MonitorTargetInput
	for code, it := range t.mInputItem {
		if code == cur {
			it.Check()
		} else {
			it.Uncheck()
		}
	}
}

func (t *Tray) refreshHosts() {
	cur := t.ctrl.Config().MouseTargetHost
	for idx, it := range t.mHostItems {
		if idx == cur {
			it.Check()
		} else {
			it.Uncheck()
		}
	}
}

// openPath opens a file with the OS default handler.
func openPath(path string) {
	switch runtime.GOOS {
	case "darwin":
		_ = exec.Command("open", path).Start()
	case "windows":
		_ = exec.Command("cmd", "/c", "start", "", path).Start()
	default:
		_ = exec.Command("xdg-open", path).Start()
	}
}
