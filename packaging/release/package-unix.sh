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

export ARCH="$GOARCH"

for component in agent control-plane relay; do
  config=packaging/linux/nfpm.yaml
  if [[ "$component" != agent ]]; then
    config="packaging/linux/nfpm-$component.yaml"
  fi
  for format in deb rpm; do
    nfpm package --config "$config" --packager "$format" \
      --target "release-assets/varde-$component-$VERSION-linux-$GOARCH.$format"
  done
done
