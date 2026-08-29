package tray

import (
	"os/exec"
	"runtime"
	"strings"
)

// notify shows a lightweight OS notification. Best-effort; failures are ignored.
func notify(title, body string) {
	switch runtime.GOOS {
	case "darwin":
		script := "display notification " + quoteAS(body) + " with title " + quoteAS(title)
		_ = exec.Command("osascript", "-e", script).Start()
	case "windows":
		// Minimal toast via PowerShell BurntToast-free balloon.
		ps := "[reflection.assembly]::loadwithpartialname('System.Windows.Forms')|Out-Null;" +
			"$n=New-Object System.Windows.Forms.NotifyIcon;" +
			"$n.Icon=[System.Drawing.SystemIcons]::Information;$n.BalloonTipTitle=" + quotePS(title) +
			";$n.BalloonTipText=" + quotePS(body) + ";$n.Visible=$true;$n.ShowBalloonTip(4000)"
		_ = exec.Command("powershell", "-NoProfile", "-Command", ps).Start()
	}
}

func quoteAS(s string) string {
	return "\"" + strings.ReplaceAll(s, "\"", "\\\"") + "\""
}

func quotePS(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}
