#!/usr/bin/env bash
# Builds Mobilehost.xcframework with gomobile, then both simulator apps.
set -euo pipefail
cd "$(dirname "$0")"
export GOWORK=off
(cd ../mobilehost && gomobile bind -target=ios,iossimulator -o ../ios/Mobilehost.xcframework .)
xcodegen generate
for scheme in MobileHostSpike MobileHostSpikeAudio; do
  xcodebuild -project MobileHostSpike.xcodeproj -scheme "$scheme" -sdk iphonesimulator \
    -configuration Release -derivedDataPath build CODE_SIGNING_ALLOWED=NO build | tail -3
done
