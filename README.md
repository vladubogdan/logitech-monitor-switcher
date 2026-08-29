# logitech-monitor-switcher

A tiny menu-bar (macOS) / system-tray (Windows) app that watches your Logitech
keyboard/mouse and, the moment they roam to another computer, **switches your
monitor's input over DDC/CI** — and optionally tells the mouse to follow the
keyboard.

It brings together, in one self-contained binary per OS, the ideas from
[CleverSwitch](https://github.com/MikalaiBarysevich/CleverSwitch) (watch the
devices, drive the monitor) and
[input-switcher](https://github.com/marcelhoffs/input-switcher) (send the mouse
a HID++ "change host" command), plus the monitor control that projects like
`ddcutil`/`m1ddc` provide — with no external tools or runtime dependencies.

## The idea

You run this on **both** computers. DDC/CI can only reach the monitor over the
link that's currently displaying, so each machine only ever performs the switch
**away**:

```
        This Mac (input A, active)                Other PC (input B)
        ┌───────────────────────────┐
        │ keyboard roams to other PC │
        │            │               │
        │            ▼               │
        │  1. push mouse to follow   │   (HID++ CHANGE HOST, feature 0x1814)
        │  2. monitor input → B      │   (DDC/CI VCP 0x60)
        └───────────────────────────┘
                     │  monitor now shows B; this Mac can no longer drive DDC
                     ▼
             Other PC's copy of the app takes over, and switches
             the monitor back to A when ITS keyboard roams away.
```

Both Logitech switch modes are handled by a single rule — **"the trigger device
left this host"**:

- **Flow / Ctrl-drag to the edge** — keyboard *and* mouse both leave. We detect
  the keyboard leaving and switch the monitor. (The mouse already left, so the
  "push mouse" command is a harmless no-op.)
- **Number keys (1/2/3)** — only the keyboard leaves. We detect that, **send the
  mouse a CHANGE HOST command so it follows**, then switch the monitor.

## How it detects a roam

The Logitech receiver stays plugged in even after a device roams away, so USB
add/remove tells us nothing. Instead the app talks HID++ to the receiver and
uses two signals together:

1. **Connection notifications** (HID++ `0x41`) — the receiver reports when a
   device's wireless link drops (roams to another host). Instant when emitted.
2. **Active ping backstop** — every ~2s it pings the trigger device; a roam is
   caught within ~2s even if no notification fires.

Safety guards so it never switches your monitor unexpectedly:

- It **only fires on a present→absent transition observed at runtime**. If a
  device is already away when the app starts, nothing happens until it comes
  back and leaves again.
- Before switching it re-confirms the device is gone (debounce + repeated pings)
  to reject momentary link blips.

## Requirements

- A Logitech **Unifying** or **Bolt** receiver (auto-detected; the app also
  accepts other Logitech receivers that expose a HID++ interface).
- A keyboard/mouse that support HID++ 2.0 **CHANGE HOST** (feature `0x1814`) if
  you want the "push mouse to follow" feature — most MX / multi-device Logitech
  gear does. The app reports at startup whether it found this.
- A monitor that honors **DDC/CI input switching** (VCP `0x60`). Enable "DDC/CI"
  in the monitor's OSD if there's a toggle.
  - On **Apple Silicon**, DDC works over **USB-C / DisplayPort**; it generally
    does **not** work over plain HDMI.

## Build

The two OS builds are separate native binaries (macOS uses IOKit; Windows uses
`dxva2.dll`), but share all the app logic. Both are single self-contained
binaries — hidapi is compiled in, so there's nothing to install at runtime.

### macOS (Apple Silicon)

Requires the Xcode command-line tools (for the C compiler) and Go 1.22+.

```bash
# plain binary
go build -o bin/logimonitorswitch ./cmd/logimonitorswitch

# or a menu-bar .app bundle (no dock icon, launch-at-login friendly)
./scripts/build-macos.sh
```

### Windows (x64)

Because hidapi and the tray use cgo, build **on Windows** with a gcc toolchain
(e.g. [MSYS2](https://www.msys2.org/) / mingw-w64) and Go:

```bat
set CGO_ENABLED=1
go build -ldflags -H=windowsgui -o logimonitorswitch.exe .\cmd\logimonitorswitch
```

`-H=windowsgui` hides the console window so it lives purely in the tray.

You can also cross-compile from macOS/Linux with mingw-w64:

```bash
CGO_ENABLED=1 GOOS=windows GOARCH=amd64 CC=x86_64-w64-mingw32-gcc \
  go build -ldflags -H=windowsgui -o logimonitorswitch.exe ./cmd/logimonitorswitch
```

## Configure

Options live in the tray menu and persist to a JSON file:

- macOS: `~/Library/Application Support/logiMonitorSwitch/config.json`
- Windows: `%AppData%\logiMonitorSwitch\config.json`

| Setting | Meaning |
|---|---|
| **Enabled** | Master on/off for automatic switching. |
| **Trigger on** | Which device leaving fires a switch (default: keyboard). |
| **Switch monitor to (on roam)** | The VCP `0x60` input to switch to — i.e. the *other* computer's input. |
| **Push mouse to follow** | Send the mouse a CHANGE HOST command on switch (needed for number-key mode). |
| **Mouse target host** | Which host slot (1/2/3) the mouse switches to. |
| **Start at login** | Toggle launch-at-login (macOS LaunchAgent / Windows HKCU Run key). |

The **Switch monitor to** menu is populated from the monitor's own DDC
capabilities string when the monitor answers capability reads (the menu title
then says "— from monitor"). When it doesn't — some panels, and Apple Silicon
DDC reads in particular, are unreliable — it falls back to a list of common VCP
`0x60` codes. Either way you can set `monitorTargetInput` to any 0–255 value in
the config file; the app is monitor-agnostic and just writes what you give it.

## Logs

The app writes a log to the app dir next to `config.json`
(`…/logiMonitorSwitch/logiMonitorSwitch.log`) — useful for the Windows build,
which has no console. It records the receiver found, its HID++ collections, the
identified devices, a CHANGE HOST self-check, and every switch.

Set the same layout consistently on both machines (e.g. host 1 = Mac, host 2 =
PC), and on each machine set "switch monitor to" = the *other* machine's input.

## Testing it safely

Switching the input **blacks out this computer's signal** until the other
computer (or the monitor's buttons) switches it back — so test deliberately:

1. Make sure the **other computer is on** and connected to its input, so you can
   switch back from there.
2. In the tray → **Test: switch monitor now** → pick the other computer's input.
   The monitor should flip. Switch back from the other machine.
3. Once you've confirmed the right input code, set it as **Switch monitor to (on
   roam)**, enable the app on both machines, and try a real keyboard switch.

> This app writes DDC but does not depend on DDC **reads** (many monitors,
> including on Apple Silicon, return unreliable reads). "Current input" display
> is therefore best-effort.

## Diagnostics

A separate probe prints what the app sees:

```bash
go run ./cmd/probe        # receiver, devices, types, CHANGE HOST support; watches for roam events
go run ./cmd/probe ddc    # DDC displays + VCP read attempt
```

## Project layout

```
cmd/logimonitorswitch   entrypoint (loads config, runs tray)
cmd/probe               diagnostic tool (not shipped)
internal/config         JSON settings
internal/hidtransport   opens the receiver's HID++ endpoint (vendored hidapi)
internal/hidpp          HID++ protocol: connection notifications + CHANGE HOST
internal/ddc            DDC/CI input switch (ddc_darwin.go / ddc_windows.go)
internal/app            detect-and-switch state machine
internal/tray           menu-bar / tray UI
```
