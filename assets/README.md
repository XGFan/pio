# App icon assets

`scripts/build-app.sh` looks here for the macOS app icon, in this order:

1. **`AppIcon.icns`** — used as-is (drop a prebuilt `.icns` here).
2. **`AppIcon.png`** — a single 1024×1024 PNG source; the build script converts
   it to `AppIcon.icns` automatically via `sips` + `iconutil`.

If neither file exists, the app builds fine but ships with the generic macOS
app icon (the build script prints a note).

The generated/bundled icon lands at `Contents/Resources/AppIcon.icns` and is
referenced by `CFBundleIconFile` in the bundle's `Info.plist`.
