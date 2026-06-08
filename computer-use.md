# Controlling PIO with Claude Code computer-use

This document captures what was verified empirically on 2026-05-17 about
making `/Applications/PIO.app` (bundle ID `com.test4x.pio`)
controllable from Claude Code's `mcp__computer-use__*` tools.

## TL;DR

To control PIO via computer-use:

```sh
PIO_NO_LSUIELEMENT=1 ./scripts/build-app.sh
pkill -x PIO; pkill -x piod
rm -rf /Applications/PIO.app
cp -R dist/PIO.app /Applications/
/System/Library/Frameworks/CoreServices.framework/Versions/A/Frameworks/LaunchServices.framework/Versions/A/Support/lsregister -f /Applications/PIO.app
open /Applications/PIO.app
```

Then in Claude Code: `request_access(["PIO"])`.

To restore the production menu-bar-only build:

```sh
./scripts/build-app.sh   # no env var
# re-deploy as above
```

## The actual filter rule

`computer-use`'s `request_access` exposes an installed-apps allowlist that is
**not** every installed application. The filter is:

> Apps with `LSUIElement = true` in Info.plist are excluded.

Signature does **not** affect inclusion. The script comment that previously
claimed Developer ID Application is required was incorrect.

### Evidence

Verified on this machine against the in-prompt `installed-apps` list:

| Sample | LSUIElement | spctl | In allowlist |
|---|---|---|---|
| iTerm, kitty, Cherry Studio, Notion, Antigravity, Codex, Zed, Discord, Telegram, Typora, IINA, CleanMyMac, QQ, Setapp, Helium, Stay, Wireshark | no | various | yes (14/14 sampled) |
| Amphetamine, Background Music, BetterDisplay, JetBrains Toolbox, OpenInTerminal-Lite, Raycast, SoundSource, Spokenly, Tailscale, TaskTab, TinyViewer, VirtualHere*, Windows App, PIO (pre-fix) | **yes** | various | **no** (14/14 sampled) |
| chiaki-ng | no | **rejected** | yes |
| KedaManga, ProxMobo, XLD | no | **rejected** | yes |
| Codex | no | cert **revoked** | yes |
| Air, BlueStacksMIM, SVP 4 Mac | no | **invalid** | yes |

Signature-rejected/invalid/revoked apps are included; LSUIElement apps are
not. A small number of non-LSUIElement apps were also missing
(Numbers Creator Studio shares `CFBundleIdentifier = com.apple.Numbers` with
Apple Numbers — looks like bundle-ID dedupe).

## Request_access mechanics

- **Name lookup**: pass the bundle's `CFBundleName` (e.g. `"PIO"`),
  not `CFBundleDisplayName` (`"PIO"`) and not the bundle ID. If
  you guess wrong it returns `didYouMean` with the canonical name.
- **Snapshot refresh**: the allowlist is **not** strictly frozen at SDK
  startup. Re-registering via `lsregister -f <app>` plus relaunching the app
  is sufficient for `request_access` to pick up changes mid-session. (No need
  to quit Claude Desktop.)
- **Granted tier**: `"full"` for PIO after the fix — clicks, keys,
  drags all work.

## Why LSUIElement was originally set

PIO is designed as a menu-bar-only app: see
`ui/PIO/Sources/PIO/App.swift`. The Info.plist
`LSUIElement = true` keeps it out of the Dock and Cmd-Tab switcher.

When `PIO_NO_LSUIELEMENT=1` is set, `scripts/build-app.sh` omits that
key. The app then shows in the Dock and behaves as a normal foreground app,
with the menu-bar extra still working as before. Quit and reopen the app for
the change to take effect.

## Operating the app via computer-use

The main window does **not** auto-open on launch — it has to be opened
explicitly. Three working paths:

1. **Window menu → "PIO"**: with the app frontmost, click the
   `Window` menu in the system menu bar. The window definition is listed at
   the bottom; clicking it opens the window. (Earlier failures were caused
   by clicking before the menu was fully open.)
2. **Menu-bar extra → "Open Window…"**: click the network icon in the menu
   bar, then the menu item.
3. **Programmatic**: AppleScript via `osascript` works only if the parent
   `osascript` binary has Accessibility permission, which it does not by
   default in this setup (`-1719` error). Not the preferred path.

The window has two tabs (`Proxy Sources`, `Users & Rules`). The `System`
section has a `Listen addr` selector and a single `Proxy port` field (the
unified HTTP+SOCKS5 port, default `8080`, bound to `127.0.0.1`), `Sync (min)`
(default 60), a `Universal password` field (with `Set`/`Not set` status and
`Save`/`Clear` buttons), an `Apply` button, and a `Start/Stop Proxy` button.
Below it are the `Webshare` API-keys section (empty by default) and a
`Manual Proxies` section.

## Multi-monitor gotchas

The Mac has three displays (`Built-in Retina Display`, `Mi Monitor`,
`U2777B`). `mcp__computer-use__screenshot` only captures one at a time —
use `switch_display` to navigate. The menu-bar extras live on the primary
display only. The first `cmd+N` attempt to open the PIO window
likely succeeded but on a different display than the one being captured;
the second pass via `Window` menu put it on the active display.

## What was changed in this repo

- `scripts/build-app.sh`:
  - Added `PIO_NO_LSUIELEMENT` env-var support (omits `LSUIElement`
    when set).
  - Replaced the misleading "Developer ID Application required for
    computer-use" comments with the actual rule.

No source code (`internal/`, `cmd/`, `ui/`) was touched.
