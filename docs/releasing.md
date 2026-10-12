# Releasing Varde

## Cut a release

1. Merge the changes to `main`, including a green **release dry run** when changing
   release workflows or packaging. Choose a SemVer version, e.g. `1.2.3` or
   `1.2.3-rc.1`.
2. Create and push the tag at the intended commit:
   ```sh
   git tag -a v1.2.3 -m 'Varde 1.2.3' <commit>
   git push origin v1.2.3
   ```
3. Create a GitHub Release for that existing tag, write the release notes, and
   publish it (mark release candidates as prereleases). The **release** workflow
   uses `release: published`, not tag pushes, so a tag alone does not publish
   binaries. Releases created with a workflow's `GITHUB_TOKEN` do not trigger a
   second workflow; publish via the GitHub UI or a maintainer's token instead.
4. Wait for the five platform builds, provenance, keyless signatures and upload.
   The version comes from the tag with its leading `v` removed. Cargo package
   versions, the tray configuration, Go version linker flags, package versions
   and archive names are set in the disposable build checkout, not committed.

Re-run a failed workflow from Actions. Uploads use `gh release upload --clobber`,
so assets with the same name are replaced. Published releases briefly exist
without assets while building. Do **not** enable GitHub immutable releases with
this design: immutability prevents `--clobber` and moving the nightly tag.

## Rolling nightly

**nightly binaries** runs daily at 04:43 UTC and via `workflow_dispatch`. It
builds the exact main commit captured when the run is triggered across all
platforms, keeping GitHub provenance's source SHA aligned with the checkout. The
version is `0.0.0-nightly.YYYYMMDD+<7-character-sha>`, with the date in UTC.

After successful builds/signing, publishing creates or updates the `nightly`
prerelease (never marked Latest), moves its lightweight tag to the built commit,
deletes all previous assets, and uploads the new set. The release notes record
the commit **only after** a successful upload; any previous success marker is
cleared before replacing assets, so interrupted same-commit retries cannot look
complete. Scheduled/publishing runs skip only if that successful-commit marker
matches main, the `<!-- nightly-signing:P -->` marker matches which
Authenticode provider is currently configured (so enabling or switching
signing rebuilds an unchanged main), and
the current asset names exactly match the recorded
`<!-- nightly-assets:[...] -->` manifest, including checksums and signature
bundles. Missing assets or a missing/malformed manifest trigger a rebuild. A
failed build or partial upload is retried on the next run, even if the tag has
already moved.
Publication is not atomic; downloads can temporarily be unavailable during the
asset replacement. Do not edit the commit or asset-manifest markers in the notes.

Concurrency serializes the entire nightly run, including manual runs, so two
nightlies cannot race. A publishing dispatch must select `main`; feature branches
can only dispatch their selected commit with `dry_run: true`. Manual dispatch defaults to dry-run mode.
Dry runs bypass the unchanged-main check and never create/move tags or releases.

## Assets

`<ver>` below is the complete version, including nightly metadata. Every
package contains `LICENSE`; the agent package always includes `varde-mesh`
**next to** `varde-agent`.

| Platform | Assets |
| --- | --- |
| Linux amd64, arm64 | `varde-<component>-<ver>-linux-<arch>.deb` and `.rpm` for each of agent, control-plane and relay; `varde-<component>-<ver>-linux-<arch>.snap` (strict confinement) for sideloading |
| Windows amd64 | `VardeSetup-<ver>.exe` (agent, mesh, tray; optional control-plane component for self-hosting) |
| macOS arm64, amd64 | `varde-<component>-<ver>-darwin-<arch>.dmg` for each of agent (contains agent + mesh), control-plane and relay |
| All platforms | `SHA256SUMS` over the payload assets; a `<filename>.sigstore.json` keyless signature bundle beside **every payload and SHA256SUMS**; `latest.json` + `latest.json.sig` update feed (below) |

Linux packages retain the existing systemd units, configuration, icons and
install/remove scripts. Packaged mesh lives in `/usr/lib/varde`, the agent's
installed fallback path. Linux binaries are built natively on Ubuntu 24.04
(including its arm runner); the Rust agent requires a compatible glibc runtime.
The Go binaries disable CGO; control-plane embeds the **production** web UI.
The Windows tray is never shipped on Linux/macOS. The Linux snaps use strict
confinement and run their daemons as root: agent state lives under
`/var/snap/varde-agent/common`, the control-plane sqlite db under
`/var/snap/varde-control-plane/common`; daemon configuration goes through
`snap set <snap> <key>=<value>` (see `packaging/linux/snap/`). The deb/rpm
packages remain the fully-supported installs. macOS ships unsigned `.dmg`
bundles per component — Gatekeeper still applies as described below.
The macOS agent passes its PID to mesh lifetime monitoring so abrupt agent exit
releases the mesh's configured ports; Linux retains kernel parent-death signaling.

## Update feed

The aggregation job also emits `latest.json`, the
[Tauri updater static manifest](https://v2.tauri.app/plugin/updater/):
`version`, `pub_date` and a `platforms` map whose entries carry the minisign
signature and download URL of each payload. The Windows installer uses the
plugin's native `windows-x86_64` key; every other package uses a custom
target key the updater ignores but the agent resolves for its own install
flavor — `linux-<x86_64|aarch64>-<component>-<deb|rpm|snap>` and
`darwin-<aarch64|x86_64>-<component>`. Entries without a signature are
omitted entirely: one incomplete entry fails the updater's whole-file
validation. `latest.json` itself is signed into `latest.json.sig`.

Clients poll the release asset, never the GitHub API:
`releases/latest/download/latest.json` for the stable channel and
`releases/download/nightly/latest.json` for nightly.

Apply paths per platform:

- **Windows**: the tray downloads `VardeSetup-<ver>.exe`, verifies its
  minisign signature and runs it `/S` — a silent NSIS install that drains
  services, swaps binaries and relaunches the tray.
- **Linux deb/rpm**: `varde-agent update` stages the verified package in
  `/var/lib/varde/update/` and drops a `.pending` marker; the packaged
  `varde-update.path` unit fires `varde-update.service`, which runs
  `varde-agent apply-update` as root (`dpkg -i` / `rpm -U`). The package
  postinst `try-restart`s the agent so the new binary takes over under the
  normal drain timeout. The snap package is notify-only — strict snaps
  self-refresh via snapd once store-published.
- **macOS**: `varde-agent update` mounts the dmg and swaps `varde-agent` +
  `varde-mesh` next to the running exe; restarting the agent (launchd or
  manual) runs the new binary.

Update signing uses a Tauri signer (minisign) keypair, stored as the
**repository** secrets `TAURI_SIGNING_PRIVATE_KEY` and
`TAURI_SIGNING_PRIVATE_KEY_PASSWORD` — repository secrets, not
environment-scoped: the aggregation job does not run under
`environment: release`, which would change its cosign OIDC subject and break
certificate verification. Generate the keypair once per installation (never
commit the private key):

```sh
bunx @tauri-apps/cli@^2 signer generate -w ~/.tauri/varde.key
# private key + password  -> the two repository secrets above
# public key              -> baked into update clients (agent, tray)
```

Losing the private key means shipping a new public key to every installed
client before updates verify again; losing the password makes the key
unusable. Without the secret the workflow still emits `latest.json`, but
with no signed platforms — clients treat those builds as unverifiable.

## Signatures and platform warnings

Every payload and SHA256SUMS receives a keyless Sigstore signature via GitHub
OIDC (`id-token: write`) and GitHub build provenance (`attestations: write`).
Only the final release upload job receives `contents: write`. These
signatures establish origin but do not replace platform-native code signing.
Every payload additionally carries a minisign `.sig` (and `latest.json.sig`)
for the update feed — the signature inside `latest.json`, not the `.sig`
asset, is what update clients consume.

Windows executables and `VardeSetup-<ver>.exe` are Authenticode-signed in the
Windows build job via [Azure Artifact Signing](https://learn.microsoft.com/en-us/azure/trusted-signing/)
(~$10/month, publisher is the validated organization name; the free SignPath
Foundation application was declined). The build signs automatically once the
`AZURE_*` secrets/variables are configured; unconfigured and `dry-run` builds
stay unsigned (setup: `packaging/windows/README.md`). Linux deb/rpm packages have no GPG
signatures, and macOS has no Apple Developer ID signing or notarization.

### Windows SmartScreen

Signed builds show the validated organization as the verified publisher
instead of an unknown publisher. SmartScreen per-file reputation still ramps per
download, so a warning may appear for fresh releases: choose **More info →
Run anyway** after verifying the download as described below. Unsigned
builds (no provider configured, or dry runs) still trigger the unsigned
warning. Managed device policies may prevent either path; do not disable
organization security controls.

### macOS Gatekeeper

Browser-downloaded binaries can be quarantined and blocked by Gatekeeper.
**After verifying origin/signatures/checksums**, remove quarantine only from
the specific extracted binaries you trust:

```sh
xattr -d com.apple.quarantine ./varde-agent ./varde-mesh
# Likewise for ./varde-control-plane and ./varde-relay when using them.
```

### Windows code signing

`varde-agent.exe`, `varde-mesh.exe`, `varde-tray.exe`,
`varde-control-plane.exe` and `varde-relay.exe` are signed **before**
NSIS packaging (`tauri build --no-build --bundles nsis`), and the installer
is signed **after** — all before checksum generation, Sigstore signing and
publication. Signing uses Azure Artifact Signing; setup is documented in
`packaging/windows/README.md`.

## Verify downloads

Install [cosign](https://docs.sigstore.dev/cosign/system_config/installation/) v2.5.2
or newer and a recent [GitHub CLI](https://cli.github.com/). Download a payload,
its `.sigstore.json` bundle, `SHA256SUMS` and `SHA256SUMS.sigstore.json`:

```sh
gh release download v1.2.3 --repo grendalaget/varde \
  --pattern 'varde-agent-1.2.3-linux-amd64.deb*' --pattern 'SHA256SUMS*'

# The identity is the reusable BUILD workflow, not the release caller.
identity='https://github.com/grendalaget/varde/.github/workflows/build-release.yml@refs/tags/v1.2.3'
cosign verify-blob --bundle SHA256SUMS.sigstore.json \
  --certificate-identity "$identity" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com SHA256SUMS
cosign verify-blob --bundle varde-agent-1.2.3-linux-amd64.deb.sigstore.json \
  --certificate-identity "$identity" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  varde-agent-1.2.3-linux-amd64.deb
sha256sum --check --ignore-missing SHA256SUMS
gh attestation verify varde-agent-1.2.3-linux-amd64.deb --repo grendalaget/varde
```

For nightly, download tag `nightly` and use the certificate identity ending in
`@refs/heads/main`, **not** `@refs/tags/nightly`. Provenance is recorded for every
payload in its native build job and for SHA256SUMS in the aggregation job.
Bundles include the certificate and transparency-log inclusion evidence; do not
disable identity/issuer verification. Checksums alone do not establish origin.

## Validate without publishing

**release dry run** runs on same-repository PRs that change the release workflows,
packaging or this document, and can be manually dispatched with a test version.
It runs actionlint + helper checks, the complete native matrix, packaging,
provenance, cosign signing **and verification**, macOS abrupt-parent-death/port
recovery tests and the agent launch-contract test, and uploads the result as the
`release-assets` Actions artifact (seven-day retention). No provider secrets
are required; Authenticode signing is skipped for `dry-run` builds. Fork PRs run lint only because their tokens cannot
write GitHub attestations; dispatch on a reviewed, trusted branch to test them.
The publishing workflow is never called by the PR dry run.

Actual upload-path testing requires maintainer approval and a disposable
prerelease; the PR validation must not publish to the upstream repo.
