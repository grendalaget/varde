#!/usr/bin/env bash
# Assemble Varde.app for release: `bunx tauri bundle` produces the app
# skeleton from the already-built varde-tray binary (same trick as the
# Windows leg: tauri.bundle.macos.conf.json points frontendDist at a staging dir),
# then varde-agent + varde-mesh land in Contents/MacOS and Info.plist gets
# the menubar + varde:// entries.
#
# Requires: dist/varde-tray (cargo build --release -p varde-tray),
#           dist/varde-agent, dist/varde-mesh, bun.
# Env: APPLE_SIGNING_IDENTITY — when set ("Developer ID Application: ..."),
#      every binary + the bundle are signed with the hardened runtime and
#      packaging/macos/entitlements.plist; unset → ad-hoc signature, which
#      is what local builds and secret-less CI runs produce.
set -euo pipefail
cd "$(dirname "$0")/../.."

APP="target/release/bundle/macos/Varde.app"

# The bundler wants a frontendDist dir; its contents were already compiled
# into the binary, so a stub is enough (same as packaging/windows).
mkdir -p apps/desktop/payload
printf 'Varde.app bundling stub — the real UI is embedded in the binary.\n' \
  > apps/desktop/payload/README.txt
cp LICENSE apps/desktop/payload/

bunx tauri bundle --config apps/desktop/tauri.bundle.macos.conf.json --bundles app

[[ -d "$APP" ]] || { echo "no $APP produced" >&2; exit 1; }

cp dist/varde-agent dist/varde-mesh "$APP/Contents/MacOS/"
cp LICENSE "$APP/Contents/Resources/"

# Info.plist carries LSUIElement (menubar-only, no dock icon),
# CFBundleURLTypes (varde:// → RunEvent::Opened) and VardeAgentLabel via
# tauri.bundle.macos.conf.json — patched at bundle time, not here.
# Sign innermost → outermost: every Mach-O, then the bundle itself.
if [[ -n "${APPLE_SIGNING_IDENTITY:-}" ]]; then
  OPTS=(--options runtime --entitlements packaging/macos/entitlements.plist \
        --timestamp --sign "$APPLE_SIGNING_IDENTITY")
  for f in "$APP"/Contents/MacOS/*; do
    codesign --force "${OPTS[@]}" "$f"
  done
  codesign --force "${OPTS[@]}" "$APP"
  codesign --verify --deep --strict "$APP"
  echo "signed Varde.app with '$APPLE_SIGNING_IDENTITY'"
else
  # Ad-hoc: keeps a stable cdhash for launchd/Gatekeeper bookkeeping.
  codesign --force --deep --sign - "$APP"
  echo "no APPLE_SIGNING_IDENTITY — ad-hoc signed Varde.app"
fi
