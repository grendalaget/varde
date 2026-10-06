"""Set release versions in the disposable CI checkout, including local lock entries."""

import json
import os
import re
from pathlib import Path

version = os.environ["VERSION"]
number = r"(?:0|[1-9][0-9]*)"
if not re.fullmatch(
    rf"{number}\.{number}\.{number}(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?",
    version,
):
    raise SystemExit("VERSION must be a SemVer version without a v prefix")

root = Path(__file__).resolve().parents[2]
manifests = [root / "Cargo.toml", root / "apps/desktop/Cargo.toml"]
for path in manifests:
    source = path.read_text()
    source, count = re.subn(r'^version = "[^"]+"$', f'version = "{version}"', source, flags=re.M)
    if count != 1:
        raise SystemExit(f"Expected exactly one version in {path}")
    path.write_text(source)

path = root / "Cargo.lock"
blocks = path.read_text().split("[[package]]")
for i, block in enumerate(blocks[1:], 1):
    if not re.search(r"^source =", block, re.M):
        blocks[i] = re.sub(r'^version = "[^"]+"$', f'version = "{version}"', block, flags=re.M)
path.write_text("[[package]]".join(blocks))

path = root / "apps/desktop/tauri.conf.json"
config = json.loads(path.read_text())
config["version"] = version
path.write_text(json.dumps(config, indent=2) + "\n")
