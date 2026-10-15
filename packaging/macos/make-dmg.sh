#!/usr/bin/env bash
# Build the styled release DMG: backdrop rendered at 2x from
# dmg-background.html (the varde.games ASCII-cloud hero via headless
# Chrome; dmg-background.svg is the qlmanage fallback), Varde.app + an
# /Applications symlink at fixed positions, brand volume icon — via
# appdmg (npx).
#
#   make-dmg.sh <path-to-Varde.app> <output.dmg>
set -euo pipefail
cd "$(dirname "$0")/../.."

APP=${1:?path to Varde.app}
OUT=${2:?output dmg path}
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

cp -R "$APP" "$WORK/Varde.app"
cp apps/desktop/icons/icon.icns "$WORK/volume.icns"

# --- render the backdrop at 2x + 1x (appdmg merges `x.png` and `x@2x.png`
# into one retina TIFF; Finder would draw a bare 2x PNG at 1:1 pixels) ---
render_chrome() {
  local chrome="$1"
  # WebGL2 needs SwiftShader when there's no GPU (CI runners) — hence no
  # --disable-gpu here. virtual-time lets the shader's one paint land.
  "$chrome" --headless=new --enable-unsafe-swiftshader --hide-scrollbars \
    --window-size=540,360 --force-device-scale-factor=2 \
    --virtual-time-budget=3000 \
    --screenshot="$WORK/dmg-background@2x.png" \
    "file://$PWD/packaging/macos/dmg-background.html" 2>/dev/null
}
render_qlmanage() {
  qlmanage -t -s 1080 -o "$WORK" "$PWD/packaging/macos/dmg-background.svg" \
    >/dev/null 2>&1 \
    && mv "$WORK/dmg-background.svg.png" "$WORK/dmg-background@2x.png"
}
BG_OK=""
for chrome in "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome" \
              "/Applications/Chromium.app/Contents/MacOS/Chromium"; do
  if [[ -x "$chrome" ]] && render_chrome "$chrome"; then BG_OK=1; break; fi
done
[[ -n "$BG_OK" ]] || render_qlmanage || true
[[ -f "$WORK/dmg-background@2x.png" ]] \
  || { echo "could not render dmg-background.svg (need Chrome or qlmanage)" >&2; exit 1; }
sips -s format png -Z 540 "$WORK/dmg-background@2x.png" \
  --out "$WORK/dmg-background.png" >/dev/null

cat > "$WORK/appdmg.json" <<EOF
{
  "title": "Varde",
  "icon": "$WORK/volume.icns",
  "background": "$WORK/dmg-background.png",
  "format": "UDZO",
  "window": {
    "size": { "width": 540, "height": 360 },
    "position": { "x": 400, "y": 240 }
  },
  "icon-size": 88,
  "contents": [
    { "x": 132, "y": 170, "type": "file", "path": "$WORK/Varde.app" },
    { "x": 408, "y": 170, "type": "link", "path": "/Applications" }
  ]
}
EOF

mkdir -p "$(dirname "$OUT")"
rm -f "$OUT"
npx --yes appdmg "$WORK/appdmg.json" "$OUT"
echo "wrote $OUT"
