#!/usr/bin/env bash
# build-extension.sh — Packages the Chrome (MV3) proxy-switcher extension into a
# Web-Store-ready .zip that contains ONLY the runtime files; dev/test artifacts
# (e2e/, test/, node_modules/, README.md, .gitignore, .omc/) are left out.
# Cross-platform (macOS/Linux); needs `zip`.
#
#   ./scripts/build-extension.sh [output-dir]   # default: ./dist
#
# Result: <output-dir>/pio-extension-<version>.zip
#   - upload to the Chrome Web Store, or
#   - drag-drop onto chrome://extensions (Developer mode), or
#   - turn into a self-hosted .crx (see extension/README.md → Packaging).

set -euo pipefail

OUT_DIR="${1:-./dist}"

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

EXT_DIR="$REPO_ROOT/extension"

# The extension's runtime files — an explicit allowlist so dev-only files never
# leak into a published package. This is the full dependency closure:
#   manifest.json → background.js (no imports) + popup.html
#   popup.html    → popup.css + popup.js
#   popup.js      → lib/parse.js
# Keep it in sync with manifest.json when runtime files are added/renamed.
FILES=(
  manifest.json
  background.js
  popup.html
  popup.css
  popup.js
  lib/parse.js
)

# Version drives the artifact name; read it straight from the manifest so the
# zip name can never disagree with what Chrome will install.
VERSION="$(sed -n 's/.*"version"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$EXT_DIR/manifest.json" | head -1)"
if [ -z "$VERSION" ]; then
  echo "ERROR: could not read \"version\" from extension/manifest.json" >&2
  exit 1
fi

echo "==> Packaging PIO Proxy Switcher v$VERSION"

# Fail fast if a declared runtime file is missing (typo / moved file) instead of
# silently shipping an incomplete extension.
for f in "${FILES[@]}"; do
  if [ ! -f "$EXT_DIR/$f" ]; then
    echo "ERROR: runtime file missing: extension/$f" >&2
    exit 1
  fi
done

mkdir -p "$OUT_DIR"
ABS_ZIP="$(cd "$OUT_DIR" && pwd)/pio-extension-$VERSION.zip"
rm -f "$ABS_ZIP"   # zip appends to an existing archive; start clean

# Zip from inside extension/ so manifest.json sits at the archive ROOT — Chrome
# rejects a package whose manifest is nested under a top-level folder.
( cd "$EXT_DIR" && zip -q "$ABS_ZIP" "${FILES[@]}" )

echo "==> Built: $OUT_DIR/pio-extension-$VERSION.zip"
if command -v unzip >/dev/null 2>&1; then
  echo "    Contents:"
  unzip -l "$ABS_ZIP" | sed 's/^/    /'
fi
