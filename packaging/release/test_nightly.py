import hashlib
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


FINGERPRINT_KEYS = ("AZURE_TSA_ENDPOINT", "AZURE_TSA_ACCOUNT", "AZURE_TSA_CERT_PROFILE")


def expected_signing(env):
    # mirror of the signing-marker computation in nightly.yml's source step
    if all(env.get(k) for k in ("AZURE_TSA_ENDPOINT", "AZURE_TSA_ACCOUNT",
                                "AZURE_TSA_CERT_PROFILE", "AZURE_CLIENT_ID")):
        provider = "azure"
    else:
        return "none"
    blob = "".join(env.get(k, "") + "\n" for k in FINGERPRINT_KEYS)
    return f"{provider}-{hashlib.sha256(blob.encode()).hexdigest()[:8]}"


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
            SIGNING="none",
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

    def test_signing_state_change_rebuilds(self):
        # enabling signing on an unchanged commit must publish signed assets
        self.env.update(AZURE_TSA_ENDPOINT="https://example.test",
                        AZURE_TSA_ACCOUNT="acct", AZURE_TSA_CERT_PROFILE="prof",
                        AZURE_CLIENT_ID="id")
        self.assertTrue(self.changed(self.notes(), PAYLOADS))
        # signed notes + configured signing -> skip
        (self.work / "output").unlink(missing_ok=True)
        self.env["SIGNING"] = expected_signing(self.env)
        self.assertFalse(self.changed(self.notes(), PAYLOADS))
        # signed notes + signing deconfigured -> rebuild unsigned
        del (self.env["AZURE_TSA_ENDPOINT"], self.env["AZURE_TSA_ACCOUNT"],
             self.env["AZURE_TSA_CERT_PROFILE"], self.env["AZURE_CLIENT_ID"])
        (self.work / "output").unlink(missing_ok=True)
        self.assertTrue(self.changed(self.notes(), PAYLOADS))
        # the rebuild above published unsigned notes
        self.env["SIGNING"] = expected_signing(self.env)
        # re-enabling (or adding the secret last) rebuilds too
        self.env.update(AZURE_TSA_ENDPOINT="https://example.test",
                        AZURE_TSA_ACCOUNT="acct", AZURE_TSA_CERT_PROFILE="prof",
                        AZURE_CLIENT_ID="id")
        (self.work / "output").unlink(missing_ok=True)
        self.assertTrue(self.changed(self.notes(), PAYLOADS))
        self.env["SIGNING"] = expected_signing(self.env)
        (self.work / "output").unlink(missing_ok=True)
        self.assertFalse(self.changed(self.notes(), PAYLOADS))
        self.env["AZURE_TSA_CERT_PROFILE"] = "new-profile"
        (self.work / "output").unlink(missing_ok=True)
        self.assertTrue(self.changed(self.notes(), PAYLOADS))


if __name__ == "__main__":
    unittest.main()
