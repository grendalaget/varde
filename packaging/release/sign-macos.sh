#!/usr/bin/env bash
set -euo pipefail

for name in APPLE_CERT_P12_BASE64 APPLE_CERT_PASSWORD APPLE_SIGNING_IDENTITY APPLE_TEAM_ID \
  APPLE_API_KEY_P8_BASE64 APPLE_API_KEY_ID APPLE_API_KEY_ISSUER; do
  if [[ -z "${!name:-}" ]]; then
    echo "::warning::$name absent: skipping Developer ID signing and notarization."
    exit 0
  fi
done

secret_dir=$(mktemp -d "$RUNNER_TEMP/apple-signing.XXXXXX")
keychain="$secret_dir/signing.keychain-db"
cleanup() {
  security delete-keychain "$keychain" >/dev/null 2>&1 || true
  rm -rf "$secret_dir"
}
trap cleanup EXIT
umask 077
printf '%s' "$APPLE_CERT_P12_BASE64" | base64 --decode > "$secret_dir/cert.p12"
printf '%s' "$APPLE_API_KEY_P8_BASE64" | base64 --decode > "$secret_dir/key.p8"
password=$(uuidgen)
security create-keychain -p "$password" "$keychain"
security set-keychain-settings -lut 21600 "$keychain"
security unlock-keychain -p "$password" "$keychain"
security import "$secret_dir/cert.p12" -k "$keychain" -P "$APPLE_CERT_PASSWORD" \
  -T /usr/bin/codesign -T /usr/bin/security
security set-key-partition-list -S apple-tool:,apple:,codesign: -s -k "$password" "$keychain"
security list-keychains -d user -s "$keychain" "$(security default-keychain -d user | tr -d '\"')"
security find-identity -v -p codesigning "$keychain" | grep -F "$APPLE_TEAM_ID" >/dev/null
for binary in dist/varde-*; do
  codesign --force --options runtime --timestamp --keychain "$keychain" \
    --sign "$APPLE_SIGNING_IDENTITY" "$binary"
  codesign --verify --strict --verbose=2 "$binary"
done

# Archives cannot be stapled; notarize the signed binaries together in a zip.
ditto -c -k --keepParent dist "$secret_dir/notarization.zip"
xcrun notarytool submit "$secret_dir/notarization.zip" \
  --key "$secret_dir/key.p8" --key-id "$APPLE_API_KEY_ID" --issuer "$APPLE_API_KEY_ISSUER" \
  --wait --timeout 30m --output-format json > "$secret_dir/result.json"
python3 -c 'import json,sys; r=json.load(open(sys.argv[1])); print("Notarization:", r["status"]); sys.exit(0 if r["status"] == "Accepted" else 1)' \
  "$secret_dir/result.json"
