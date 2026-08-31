#!/usr/bin/env bash
# Build logiMonitorSwitch as a menu-bar .app bundle (no dock icon).
set -euo pipefail
cd "$(dirname "$0")/.."

APP="logiMonitorSwitch"
OUT="dist/${APP}.app"
BIN="${OUT}/Contents/MacOS/${APP}"

STAMP="$(date '+%Y-%m-%d %H:%M:%S %Z')"

echo "Building binary (${STAMP})..."
mkdir -p "${OUT}/Contents/MacOS" "${OUT}/Contents/Resources"
CGO_ENABLED=1 go build -ldflags "-X 'main.buildStamp=${STAMP}'" -o "${BIN}" ./cmd/logimonitorswitch
# Refresh the bundle's own mtime so Finder shows the real build date, not the
# date the .app directory was first created.
touch "${OUT}"

echo "Writing Info.plist..."
cat > "${OUT}/Contents/Info.plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>CFBundleName</key><string>${APP}</string>
  <key>CFBundleDisplayName</key><string>${APP}</string>
  <key>CFBundleIdentifier</key><string>dev.local.logimonitorswitch</string>
  <key>CFBundleVersion</key><string>1.0.0</string>
  <key>CFBundleShortVersionString</key><string>1.0.0</string>
  <key>CFBundleExecutable</key><string>${APP}</string>
  <key>CFBundlePackageType</key><string>APPL</string>
  <!-- LSUIElement hides the dock icon: menu-bar-only agent app. -->
  <key>LSUIElement</key><true/>
  <key>LSMinimumSystemVersion</key><string>12.0</string>
</dict>
</plist>
PLIST

echo "Done: ${OUT}"
echo
echo "Run it:            open \"${OUT}\""
echo "Launch at login:   System Settings → General → Login Items → +  (add the .app)"
