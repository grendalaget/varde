import json
import tempfile
import unittest
from pathlib import Path

from update_manifest import build_manifest, platform_key


ASSETS = [
    "VardeSetup-1.2.3.exe",
    "varde-agent-1.2.3-linux-amd64.deb",
    "varde-agent-1.2.3-linux-amd64.rpm",
    "varde-agent-1.2.3-linux-amd64.snap",
    "varde-control-plane-1.2.3-linux-arm64.deb",
    "varde-relay-1.2.3-darwin-arm64.dmg",
    "varde-agent-1.2.3-darwin-amd64.dmg",
    "SHA256SUMS",
    "SHA256SUMS.sigstore.json",
]


class PlatformKeyTests(unittest.TestCase):
    def test_maps_every_payload_and_no_sidecars(self):
        self.assertEqual(platform_key("VardeSetup-0.1.0.exe"), "windows-x86_64")
        self.assertEqual(
            platform_key("varde-agent-0.1.0-linux-arm64.deb"), "linux-aarch64-agent-deb"
        )
        self.assertEqual(
            platform_key("varde-relay-0.0.0-nightly.20261012+abc1234-linux-amd64.snap"),
            "linux-x86_64-relay-snap",
        )
        self.assertEqual(
            platform_key("varde-control-plane-1.2.3-darwin-amd64.dmg"),
            "darwin-x86_64-control-plane",
        )
        for sidecar in ["SHA256SUMS", "x.sigstore.json", "x.sig", "latest.json"]:
            self.assertIsNone(platform_key(sidecar))


class BuildManifestTests(unittest.TestCase):
    def test_signed_assets_map_to_platform_urls(self):
        with tempfile.TemporaryDirectory() as temp:
            assets = Path(temp)
            for name in ASSETS:
                (assets / name).write_text("payload")
                (assets / f"{name}.sig").write_text("SIG-" + name)
            manifest, unsigned = build_manifest(assets, "1.2.3", "release", "grendalaget/varde")
            self.assertEqual(unsigned, [])
            self.assertEqual(manifest["version"], "1.2.3")
            base = "https://github.com/grendalaget/varde/releases/download/v1.2.3"
            self.assertEqual(
                manifest["platforms"]["windows-x86_64"],
                {"signature": "SIG-VardeSetup-1.2.3.exe", "url": f"{base}/VardeSetup-1.2.3.exe"},
            )
            self.assertEqual(
                manifest["platforms"]["linux-x86_64-agent-deb"]["url"],
                f"{base}/varde-agent-1.2.3-linux-amd64.deb",
            )
            self.assertEqual(len(manifest["platforms"]), 7)

    def test_unsigned_assets_are_dropped(self):
        with tempfile.TemporaryDirectory() as temp:
            assets = Path(temp)
            for name in ASSETS:
                (assets / name).write_text("payload")
            (assets / "VardeSetup-1.2.3.exe.sig").write_text("SIG")
            manifest, unsigned = build_manifest(assets, "1.2.3", "nightly", "grendalaget/varde")
            self.assertEqual(list(manifest["platforms"]), ["windows-x86_64"])
            self.assertEqual(len(unsigned), 6)

    def test_nightly_tag_uses_rolling_release(self):
        with tempfile.TemporaryDirectory() as temp:
            assets = Path(temp)
            (assets / "VardeSetup-n.exe").write_text("payload")
            (assets / "VardeSetup-n.exe.sig").write_text("SIG")
            manifest, _ = build_manifest(assets, "0.0.0-nightly.1+x", "nightly", "o/r")
            self.assertIn(
                "/releases/download/nightly/", manifest["platforms"]["windows-x86_64"]["url"]
            )

    def test_asset_names_are_url_escaped(self):
        with tempfile.TemporaryDirectory() as temp:
            assets = Path(temp)
            name = "varde-agent-0.0.0-nightly.1+abcdef-linux-amd64.deb"
            (assets / name).write_text("payload")
            (assets / (name + ".sig")).write_text("SIG")
            manifest, _ = build_manifest(assets, "0.0.0-nightly.1+abcdef", "nightly", "o/r")
            url = manifest["platforms"]["linux-x86_64-agent-deb"]["url"]
            self.assertTrue(url.endswith("/varde-agent-0.0.0-nightly.1%2Babcdef-linux-amd64.deb"))
            self.assertNotIn("+", url)


if __name__ == "__main__":
    unittest.main()
