#!/usr/bin/env bash
set -euo pipefail

: "${VERSION:?}" "${GOOS:?}" "${GOARCH:?}"
mkdir -p release-assets

if [[ "$GOOS" == darwin ]]; then
  for component in agent control-plane relay; do
    staging=$(mktemp -d)
    files=("varde-$component" LICENSE)
    if [[ "$component" == agent ]]; then
      files+=(varde-mesh)
    fi
    for file in "${files[@]}"; do
      cp "dist/$file" "$staging/"
    done
    hdiutil create -volname "varde-$component" -srcfolder "$staging" -ov \
      -format UDZO "release-assets/varde-$component-$VERSION-darwin-$GOARCH.dmg"
    rm -rf "$staging"
  done
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

# Snap versions cap at 32 chars; nightly VERSION carries a long +sha suffix.
SNAP_VERSION="${VERSION%%+*}"
SNAP_VERSION="${SNAP_VERSION:0:32}"

for component in agent control-plane relay; do
  project="packaging/linux/snap/$component"
  rm -rf "$project/dist"
  mkdir -p "$project/dist"
  files=("varde-$component" LICENSE)
  if [[ "$component" == agent ]]; then
    files+=(varde-mesh)
  fi
  for file in "${files[@]}"; do
    cp "dist/$file" "$project/dist/"
  done
  printf '%s\n' "$SNAP_VERSION" > "$project/version.txt"
  (cd "$project" && snapcraft pack --destructive-mode)
  mv "$project"/varde-"$component"_*.snap \
    "release-assets/varde-$component-$VERSION-linux-$GOARCH.snap"
done
