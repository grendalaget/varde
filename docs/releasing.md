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
complete. Scheduled/publishing runs skip if
that successful-commit marker matches main and assets exist. A failed build or
partial upload is retried on the next run, even if the tag has already moved.
Publication is not atomic; downloads can temporarily be unavailable during the
asset replacement. Do not delete the commit marker from the nightly notes.

Concurrency serializes the entire nightly run, including manual runs, so two
nightlies cannot race. A publishing dispatch must select `main`; feature branches
can only dispatch their selected commit with `dry_run: true`. Manual dispatch defaults to dry-run mode.
Dry runs bypass the unchanged-main check and never create/move tags or releases.

## Assets

`<ver>` below is the complete version, including nightly metadata. Each tarball
contains `LICENSE` and the named binaries at its root. The agent tarball includes
`varde-mesh` **next to** `varde-agent`; keep them together when extracting it.

| Platform | Assets |
| --- | --- |
| Linux amd64, arm64 | `varde-agent-<ver>-linux-<arch>.tar.gz` (agent + mesh), `varde-control-plane-<ver>-linux-<arch>.tar.gz`, `varde-relay-<ver>-linux-<arch>.tar.gz`; each component also has matching `.deb` and `.rpm` files |
| Windows amd64 | `VardeSetup-<ver>.exe`; `varde-<ver>-windows-amd64.zip` (agent, mesh, tray, control-plane, relay, LICENSE) |
| macOS arm64, amd64 | `varde-agent-<ver>-darwin-<arch>.tar.gz` (agent + mesh), `varde-control-plane-<ver>-darwin-<arch>.tar.gz`, `varde-relay-<ver>-darwin-<arch>.tar.gz` |
| All platforms | `SHA256SUMS` over the payload assets; a `<filename>.sigstore.json` keyless signature bundle beside **every payload and SHA256SUMS** |

Linux packages retain the existing systemd units, configuration, icons and
install/remove scripts. Packaged mesh lives in `/usr/lib/varde`, the agent's
installed fallback path. Linux binaries are built natively on Ubuntu 24.04
(including its arm runner); the Rust agent requires a compatible glibc runtime.
The Go binaries disable CGO; control-plane embeds the **production** web UI.
The Windows tray is never shipped on Linux/macOS. macOS installers, apps and
disk images are intentionally deferred; only CLI-style tarballs are produced.

## Signatures and platform warnings

**No signing-provider secrets are needed.** Every payload and SHA256SUMS receives
a keyless Sigstore signature via GitHub OIDC (`id-token: write`) and GitHub
build provenance (`attestations: write`). Only the final release upload job
receives `contents: write`. These signatures establish origin but do not
replace platform-native code signing or suppress OS warnings.

Platform code signing is deliberately deferred: Windows executables/installers
are unsigned, Linux deb/rpm packages have no GPG signatures, and macOS has no
Apple Developer ID signing or notarization.

### Windows SmartScreen

The unsigned installer will trigger a Windows SmartScreen warning. After
verifying the download as described below, choose **More info → Run anyway** to
approve the installer manually. Managed device policies may prevent this;
do not disable organization security controls.

### macOS Gatekeeper

Browser-downloaded binaries can be quarantined and blocked by Gatekeeper.
**After verifying origin/signatures/checksums**, remove quarantine only from
the specific extracted binaries you trust:

```sh
xattr -d com.apple.quarantine ./varde-agent ./varde-mesh
# Likewise for ./varde-control-plane and ./varde-relay when using them.
```

### Future: Windows code signing via SignPath Foundation

[SignPath Foundation](https://signpath.org) offers free OSS code signing.
When adopted, sign `varde-agent.exe`, `varde-mesh.exe` and `varde-tray.exe`
**before ISCC/zip packaging**, then sign the resulting installer **after ISCC**
and before checksum generation, Sigstore signing and publication. Foundation
enrollment and artifact/origin-verification configuration belong to that
future change; no SignPath action, PFX/SignTool adapter or paid provider is
implemented here.

## Verify downloads

Install [cosign](https://docs.sigstore.dev/cosign/system_config/installation/) v2.5.2
or newer and a recent [GitHub CLI](https://cli.github.com/). Download a payload,
its `.sigstore.json` bundle, `SHA256SUMS` and `SHA256SUMS.sigstore.json`:

```sh
gh release download v1.2.3 --repo grendalaget/varde \
  --pattern 'varde-agent-1.2.3-linux-amd64.tar.gz*' --pattern 'SHA256SUMS*'

# The identity is the reusable BUILD workflow, not the release caller.
identity='https://github.com/grendalaget/varde/.github/workflows/build-release.yml@refs/tags/v1.2.3'
cosign verify-blob --bundle SHA256SUMS.sigstore.json \
  --certificate-identity "$identity" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com SHA256SUMS
cosign verify-blob --bundle varde-agent-1.2.3-linux-amd64.tar.gz.sigstore.json \
  --certificate-identity "$identity" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  varde-agent-1.2.3-linux-amd64.tar.gz
sha256sum --check --ignore-missing SHA256SUMS
gh attestation verify varde-agent-1.2.3-linux-amd64.tar.gz --repo grendalaget/varde
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
provenance, cosign signing **and verification**, and uploads the result as the
`release-assets` Actions artifact (seven-day retention). No provider secrets
are required. Fork PRs run lint only because their tokens cannot
write GitHub attestations; dispatch on a reviewed, trusted branch to test them.
The publishing workflow is never called by the PR dry run.

Actual upload-path testing requires maintainer approval and a disposable
prerelease; the PR validation must not publish to the upstream repo.
