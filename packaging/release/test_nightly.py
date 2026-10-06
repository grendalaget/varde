import json
import os
import re
import subprocess
import tempfile
import textwrap
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
SHA = "123456789abcdef"
PAYLOADS = ["VardeSetup-test.exe", "SHA256SUMS", "SHA256SUMS.sigstore.json"]


def run_block(workflow, step):
    source = (ROOT / ".github/workflows" / workflow).read_text().split(step, 1)[1]
    match = re.search(r"        run: \|\n((?:          [^\n]*\n|\n)+)", source)
    return textwrap.dedent(match[1])


class NightlyTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.work = Path(self.temp.name)
        for directory in ["bin", "release-assets"]:
            (self.work / directory).mkdir()
        scripts = {
            "git": "#!/bin/sh\nprintf '%s\\n' \"$MOCK_SHA\"\n",
            "gh": """#!/bin/sh
if [ "$1 $2" = 'release view' ]; then
  cat "$MOCK_RELEASE"
elif [ "$1 $2" = 'release edit' ]; then
  cp notes.md "$MOCK_NOTES"
else
  exit 1
fi
""",
        }
        for name, script in scripts.items():
            path = self.work / "bin" / name
            path.write_text(script)
            path.chmod(0o700)
        self.env = dict(
            os.environ,
            PATH=f"{self.work / 'bin'}:{os.environ['PATH']}",
            MOCK_SHA=SHA,
            MOCK_RELEASE=str(self.work / "release.json"),
            MOCK_NOTES=str(self.work / "published-notes.md"),
            GITHUB_OUTPUT=str(self.work / "output"),
            GITHUB_STEP_SUMMARY=str(self.work / "summary"),
            DRY_RUN="false",
            VERSION="0.0.0-nightly.20261006+1234567",
            COMMIT=SHA,
        )

    def notes(self):
        for name in PAYLOADS:
            (self.work / "release-assets" / name).touch()
        script = run_block("publish-release.yml", "      - name: Mark nightly commit")
        subprocess.run(["bash", "-e", "-o", "pipefail", "-c", script],
                       cwd=self.work, env=self.env, check=True, capture_output=True)
        return (self.work / "published-notes.md").read_text()

    def changed(self, body, assets):
        release = {"body": body, "assets": [{"name": name} for name in assets]}
        (self.work / "release.json").write_text(json.dumps(release))
        script = run_block("nightly.yml", "      - id: source")
        result = subprocess.run(["bash", "-e", "-o", "pipefail", "-c", script],
                                cwd=self.work, env=self.env, capture_output=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        return "changed=true" in (self.work / "output").read_text()

    def test_complete_unchanged_release_skips(self):
        self.assertFalse(self.changed(self.notes(), list(reversed(PAYLOADS))))

    def test_each_missing_asset_rebuilds(self):
        for missing in PAYLOADS:
            with self.subTest(missing=missing):
                (self.work / "output").unlink(missing_ok=True)
                self.assertTrue(self.changed(self.notes(), [n for n in PAYLOADS if n != missing]))

    def test_empty_asset_set_rebuilds(self):
        self.assertTrue(self.changed(self.notes(), []))

    def test_unexpected_asset_rebuilds(self):
        self.assertTrue(self.changed(self.notes(), PAYLOADS + ["stale.zip"]))

    def test_legacy_notes_without_manifest_rebuild(self):
        self.assertTrue(self.changed(f"<!-- nightly-commit:{SHA} -->", PAYLOADS))

    def test_malformed_manifest_rebuilds(self):
        self.assertTrue(self.changed(
            f"<!-- nightly-commit:{SHA} -->\n<!-- nightly-assets:[bad-json] -->", PAYLOADS))

    def test_changed_commit_rebuilds(self):
        self.assertTrue(self.changed(self.notes().replace(SHA, "old-sha"), PAYLOADS))

    def test_dry_run_bypasses_complete_release(self):
        self.env["DRY_RUN"] = "true"
        self.assertTrue(self.changed(self.notes(), PAYLOADS))


if __name__ == "__main__":
    unittest.main()
