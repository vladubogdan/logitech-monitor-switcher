// Command logimonitorswitch is a menu-bar / system-tray app that watches the
// Logitech keyboard/mouse and, when they roam to another computer, pushes the
// mouse to follow and switches the monitor's input over DDC/CI.
package main

import (
	"flag"
	"io"
	"log"
	"os"
	"path/filepath"

	"logimonitorswitch/internal/config"
	"logimonitorswitch/internal/singleton"
	"logimonitorswitch/internal/tray"
)

func main() {
	cfgPath := flag.String("config", "", "path to config file (default: OS user config dir)")
	flag.Parse()

	path := *cfgPath
	if path == "" {
		p, err := config.DefaultPath()
		if err != nil {
			log.Fatalf("resolve config path: %v", err)
		}
		path = p
	}

	logFile := setupLogging(filepath.Dir(path))
	if logFile != nil {
		defer logFile.Close()
	}

	if !singleton.Acquire() {
		log.Print("another instance is already running; exiting")
		return
	}

	cfg, err := config.Load(path)
	if err != nil {
		log.Fatalf("load config %s: %v", path, err)
	}
	log.Printf("started; config: %s", cfg.Path())

	tray.Run(cfg)
}

// setupLogging tees the log to a rotating-ish file in the app dir, so the
// Windows GUI build (which has no console) still leaves a diagnosable trail.
func setupLogging(dir string) *os.File {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil
	}
	p := filepath.Join(dir, "logiMonitorSwitch.log")
	// Truncate if it has grown past ~1 MB.
	if fi, err := os.Stat(p); err == nil && fi.Size() > 1<<20 {
		_ = os.Truncate(p, 0)
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil
	}
	log.SetOutput(io.MultiWriter(os.Stderr, f))
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.Printf("=== logiMonitorSwitch starting (log: %s) ===", p)
	return f
}
