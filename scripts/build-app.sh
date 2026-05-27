#!/usr/bin/env bash
# build-app.sh — Builds the Go daemon + SwiftUI menubar app and packages
# them into WebshareProxy.app. macOS only.
#
#   ./scripts/build-app.sh [output-dir]   # default: ./dist
#
# Result: <output-dir>/WebshareProxy.app — drag to /Applications.

set -euo pipefail

OUT_DIR="${1:-./dist}"
APP_BUNDLE="$OUT_DIR/WebshareProxy.app"
MACOS_DIR="$APP_BUNDLE/Contents/MacOS"
RESOURCES_DIR="$APP_BUNDLE/Contents/Resources"

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

echo "==> Building Go daemon (static, CGO_ENABLED=0)"
CGO_ENABLED=0 go build -ldflags="-s -w" -o "$REPO_ROOT/build/webshare-proxyd" ./cmd/webshare-proxyd

echo "==> Building SwiftUI app (release)"
( cd ui/WebshareProxy && swift build -c release )
SWIFT_BIN="$REPO_ROOT/ui/WebshareProxy/.build/release/WebshareProxy"

echo "==> Assembling $APP_BUNDLE"
rm -rf "$APP_BUNDLE"
mkdir -p "$MACOS_DIR" "$RESOURCES_DIR"

cp "$SWIFT_BIN" "$MACOS_DIR/WebshareProxy"
cp "$REPO_ROOT/build/webshare-proxyd" "$MACOS_DIR/webshare-proxyd"
chmod +x "$MACOS_DIR/WebshareProxy" "$MACOS_DIR/webshare-proxyd"

# App icon: provide either assets/AppIcon.icns (used as-is) or assets/AppIcon.png
# (1024x1024 source, converted here via sips/iconutil). If neither exists, the
# app ships without a custom icon and macOS shows the generic app icon.
# ICON_FILE_LINE is injected into Info.plist below only when an icon is bundled.
ASSETS_DIR="$REPO_ROOT/assets"
ICON_FILE_LINE=''
if [ -f "$ASSETS_DIR/AppIcon.icns" ]; then
  echo "==> Bundling icon: assets/AppIcon.icns"
  cp "$ASSETS_DIR/AppIcon.icns" "$RESOURCES_DIR/AppIcon.icns"
  ICON_FILE_LINE='  <key>CFBundleIconFile</key>       <string>AppIcon</string>'
elif [ -f "$ASSETS_DIR/AppIcon.png" ]; then
  echo "==> Generating icon from assets/AppIcon.png"
  ICONSET_PARENT="$(mktemp -d)"
  ICONSET="$ICONSET_PARENT/AppIcon.iconset"
  mkdir -p "$ICONSET"
  # px:filename pairs covering the standard macOS iconset sizes (1x + 2x).
  for spec in \
    "16:icon_16x16"    "32:icon_16x16@2x" \
    "32:icon_32x32"    "64:icon_32x32@2x" \
    "128:icon_128x128" "256:icon_128x128@2x" \
    "256:icon_256x256" "512:icon_256x256@2x" \
    "512:icon_512x512" "1024:icon_512x512@2x"; do
    px="${spec%%:*}"; name="${spec##*:}"
    sips -z "$px" "$px" "$ASSETS_DIR/AppIcon.png" --out "$ICONSET/$name.png" >/dev/null
  done
  iconutil -c icns "$ICONSET" -o "$RESOURCES_DIR/AppIcon.icns"
  rm -rf "$ICONSET_PARENT"
  ICON_FILE_LINE='  <key>CFBundleIconFile</key>       <string>AppIcon</string>'
else
  echo "==> No icon found (assets/AppIcon.icns or assets/AppIcon.png); using generic app icon"
fi

# Set WEBSHARE_NO_LSUIELEMENT=1 to drop the LSUIElement key, producing a
# Dock-visible app. Needed when Claude Code's computer-use MCP must control
# the UI — its installed-apps snapshot filters out LSUIElement (menu-bar-only)
# apps.
LSUI_LINE='  <key>LSUIElement</key>             <true/>'
if [ -n "${WEBSHARE_NO_LSUIELEMENT:-}" ]; then
  echo "==> WEBSHARE_NO_LSUIELEMENT set: omitting LSUIElement (app will show in Dock)"
  LSUI_LINE=''
fi

cat > "$APP_BUNDLE/Contents/Info.plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>CFBundleExecutable</key>      <string>WebshareProxy</string>
  <key>CFBundleIdentifier</key>      <string>com.guofan.webshare-proxy</string>
  <key>CFBundleName</key>            <string>WebshareProxy</string>
  <key>CFBundleDisplayName</key>     <string>Webshare Proxy</string>
  <key>CFBundleVersion</key>         <string>0.1.0</string>
  <key>CFBundleShortVersionString</key><string>0.1.0</string>
  <key>CFBundlePackageType</key>     <string>APPL</string>
  <key>LSMinimumSystemVersion</key>  <string>13.0</string>
${ICON_FILE_LINE}
${LSUI_LINE}
  <key>NSHumanReadableCopyright</key><string>Local-only proxy aggregator</string>
  <key>CFBundleURLTypes</key>
  <array>
    <dict>
      <key>CFBundleURLName</key>     <string>com.guofan.webshare-proxy.url</string>
      <key>CFBundleURLSchemes</key>  <array><string>webshareproxy</string></array>
    </dict>
  </array>
</dict>
</plist>
PLIST

# Codesign: prefer Developer ID Application (production), fall back to Apple
# Development (free Apple ID, dev-only). Without either, keep the adhoc
# signature swift build produced. Override with WEBSHARE_SIGN_IDENTITY.
#
# Note: Developer ID Application is required only for Gatekeeper (spctl
# --assess) acceptance on other machines. It is NOT required for Claude Code's
# computer-use MCP — that filter is LSUIElement, not signature (verified
# empirically: spctl-rejected apps appear in computer-use's allowlist when
# they don't set LSUIElement). For computer-use control, use
# WEBSHARE_NO_LSUIELEMENT=1.
SIGN_IDENTITY="${WEBSHARE_SIGN_IDENTITY:-}"
if [ -z "$SIGN_IDENTITY" ]; then
  SIGN_IDENTITY=$(security find-identity -v -p codesigning 2>/dev/null \
    | awk -F\" '/Developer ID Application/ {print $2; exit}')
fi
if [ -z "$SIGN_IDENTITY" ]; then
  SIGN_IDENTITY=$(security find-identity -v -p codesigning 2>/dev/null \
    | awk -F\" '/Apple Development/ {print $2; exit}')
fi

if [ -n "$SIGN_IDENTITY" ]; then
  echo "==> Codesigning with: $SIGN_IDENTITY"
  codesign --force --deep --options runtime --sign "$SIGN_IDENTITY" "$APP_BUNDLE"
  codesign -v --verify "$APP_BUNDLE"
  echo "    Signed. $(codesign -dv "$APP_BUNDLE" 2>&1 | grep TeamIdentifier || true)"
  if ! spctl --assess --type execute "$APP_BUNDLE" >/dev/null 2>&1; then
    echo "    NOTE: spctl rejects this signature (typical for Apple Development certs)."
    echo "    Gatekeeper will warn on first launch on other machines."
    echo "    This does NOT block Claude Code computer-use; see WEBSHARE_NO_LSUIELEMENT."
  fi
else
  echo "==> No Developer ID / Apple Development identity found; keeping adhoc signature."
  echo "    Run 'security find-identity -v -p codesigning' to inspect available certs."
fi

echo
echo "==> Built: $APP_BUNDLE"
echo "    Run via: open '$APP_BUNDLE'"
echo "    Or directly: '$MACOS_DIR/WebshareProxy'"
