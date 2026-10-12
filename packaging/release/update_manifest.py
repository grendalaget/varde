"""Emit latest.json, the Tauri-updater static manifest, into release-assets/.

Every payload asset maps to an updater "platform" key. The Windows installer
uses the plugin's native target name (`windows-x86_64`); the agent resolves
its own install flavor from custom target keys
(`linux-<arch>-<component>-<pkg>`, `darwin-<arch>-<component>`).

An entry is only emitted with its minisign .sig (written beside the asset by
the workflow's signing step). Entries without a signature are dropped: one
incomplete platform entry fails the updater's whole-file validation.

Env: VERSION (SemVer, no leading v), CHANNEL (release|nightly|dry-run),
GH_REPO (owner/name). Usage: update_manifest.py [release-assets dir]
"""

import json
import os
import re
import sys
import urllib.parse
from datetime import datetime, timezone
from pathlib import Path

ASSET_PLATFORMS = [
    (re.compile(r"^VardeSetup-.+\.exe$"), "windows-x86_64"),
    (
        re.compile(r"^varde-(agent|control-plane|relay)-.+-linux-amd64\.(deb|rpm|snap)$"),
        "linux-x86_64-{0}-{1}",
    ),
    (
        re.compile(r"^varde-(agent|control-plane|relay)-.+-linux-arm64\.(deb|rpm|snap)$"),
        "linux-aarch64-{0}-{1}",
    ),
    (
        re.compile(r"^varde-(agent|control-plane|relay)-.+-darwin-amd64\.dmg$"),
        "darwin-x86_64-{0}",
    ),
    (
        re.compile(r"^varde-(agent|control-plane|relay)-.+-darwin-arm64\.dmg$"),
        "darwin-aarch64-{0}",
    ),
]


def platform_key(name):
    for pattern, fmt in ASSET_PLATFORMS:
        match = pattern.match(name)
        if match:
            return fmt.format(*match.groups())
    return None


def release_tag(version, channel):
    return "nightly" if channel == "nightly" else f"v{version}"


def build_manifest(assets_dir, version, channel, repo):
    base = f"https://github.com/{repo}/releases/download/{release_tag(version, channel)}"
    platforms = {}
    unsigned = []
    for asset in sorted(assets_dir.iterdir()):
        key = platform_key(asset.name)
        if key is None:
            continue  # sidecars: SHA256SUMS, *.sigstore.json, *.sig, latest.json
        sig_path = asset.with_name(asset.name + ".sig")
        signature = sig_path.read_text().strip() if sig_path.exists() else ""
        if not signature:
            unsigned.append(asset.name)
            continue
        # Nightly versions carry a build hash ("...+<sha>"); a raw '+' in the
        # URL path decodes as a space, so percent-encode the asset name.
        platforms[key] = {
            "signature": signature,
            "url": f"{base}/{urllib.parse.quote(asset.name)}",
        }
    return {
        "version": version,
        "notes": "",
        "pub_date": datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
        "platforms": platforms,
    }, unsigned


def main():
    assets_dir = Path(sys.argv[1]) if len(sys.argv) > 1 else Path("release-assets")
    manifest, unsigned = build_manifest(
        assets_dir, os.environ["VERSION"], os.environ.get("CHANNEL", "release"), os.environ["GH_REPO"]
    )
    (assets_dir / "latest.json").write_text(json.dumps(manifest, indent=2) + "\n")
    if unsigned:
        print(f"unsigned assets excluded from manifest: {', '.join(unsigned)}", file=sys.stderr)
    print(f"latest.json: {len(manifest['platforms'])} platform entries")


if __name__ == "__main__":
    main()
