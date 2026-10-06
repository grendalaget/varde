#!/usr/bin/env bash
set -euo pipefail

: "${VERSION:?}" "${GOOS:?}" "${GOARCH:?}"
mkdir -p release-assets
for component in agent control-plane relay; do
  files=("varde-$component" LICENSE)
  if [[ "$component" == agent ]]; then
    files+=(varde-mesh)
  fi
  tar -czf "release-assets/varde-$component-$VERSION-$GOOS-$GOARCH.tar.gz" -C dist "${files[@]}"
done

if [[ "$GOOS" != linux ]]; then
  exit 0
fi

key_file="$RUNNER_TEMP/nfpm-signing-key.asc"
trap 'rm -f "$key_file"' EXIT
if [[ -n "${NFPM_GPG_KEY:-}" ]]; then
  umask 077
  printf '%s' "$NFPM_GPG_KEY" > "$key_file"
else
  echo '::warning::NFPM_GPG_KEY absent: deb/rpm packages will not be GPG signed.'
fi
export ARCH="$GOARCH" NFPM_PASSPHRASE="${NFPM_GPG_PASSPHRASE:-}"

for component in agent control-plane relay; do
  config=packaging/linux/nfpm.yaml
  if [[ "$component" != agent ]]; then
    config="packaging/linux/nfpm-$component.yaml"
  fi
  if [[ -n "${NFPM_GPG_KEY:-}" ]]; then
    signed_config="$RUNNER_TEMP/nfpm-$component.yaml"
    cp "$config" "$signed_config"
    printf '\ndeb:\n  signature:\n    key_file: %s\nrpm:\n  signature:\n    key_file: %s\n' \
      "$key_file" "$key_file" >> "$signed_config"
    config="$signed_config"
  fi
  for format in deb rpm; do
    nfpm package --config "$config" --packager "$format" \
      --target "release-assets/varde-$component-$VERSION-linux-$GOARCH.$format"
  done
done
