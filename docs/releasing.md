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
resolves `main` once, then builds that exact commit across all platforms. The
version is `0.0.0-nightly.YYYYMMDD+<7-character-sha>`, with the date in UTC.

After successful builds/signing, publishing creates or updates the `nightly`
prerelease (never marked Latest), moves its lightweight tag to the built commit,
deletes all previous assets, and uploads the new set. The release notes record
the commit **only after** a successful upload. Scheduled/publishing runs skip if
that successful-commit marker matches main and assets exist. A failed build or
partial upload is retried on the next run, even if the tag has already moved.
Publication is not atomic; downloads can temporarily be unavailable during the
asset replacement. Do not delete the commit marker from the nightly notes.

Concurrency serializes the entire nightly run, including manual runs, so two
nightlies cannot race. A publishing dispatch must select `main`; feature branches
can only dispatch with `dry_run: true`. Manual dispatch defaults to dry-run mode.
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

## Signing configuration

Set repository Actions secrets under **Settings → Secrets and variables → Actions**.
Never put certificates, API keys or private keys in the checkout. Missing platform
credentials produce a visible warning and unsigned platform binaries/packages,
but publishing continues. Once configured, any signing/notarization failure is
fatal, rather than silently publishing an unsigned fallback.

| Type | Exact secrets |
| --- | --- |
| Windows Authenticode | `WINDOWS_CERT_PFX_BASE64` (base64 PFX including private key), `WINDOWS_CERT_PASSWORD` |
| Apple Developer ID + notarization | `APPLE_CERT_P12_BASE64` (base64 Developer ID Application certificate/private key), `APPLE_CERT_PASSWORD`, `APPLE_SIGNING_IDENTITY` (full identity, e.g. `Developer ID Application: Name (TEAMID)`), `APPLE_TEAM_ID`, `APPLE_API_KEY_P8_BASE64` (base64 App Store Connect `.p8` private key), `APPLE_API_KEY_ID`, `APPLE_API_KEY_ISSUER` (issuer UUID) |
| Linux package GPG | `NFPM_GPG_KEY` (ASCII-armored exported **secret** signing key), `NFPM_GPG_PASSPHRASE` (optional for unencrypted keys; required when the key is encrypted) |
| Sigstore / GitHub provenance | **No secrets.** GitHub OIDC is used with `id-token: write`; attestations use `attestations: write`. Only the upload job receives `contents: write`. |

Windows signs the raw `.exe` files before creating the zip/installer, then signs
the installer with SHA-256 and an RFC 3161 DigiCert timestamp. Both steps use the
single provider adapter in `packaging/release/windows-sign/action.yml`. To adopt
Azure Trusted Signing later, replace that adapter's implementation with the
Azure signing action/authentication; the two callers and packaging order remain
unchanged. Azure credentials/configuration are not assumed by this PR.

macOS imports the certificate into a temporary keychain, signs each Mach-O with
Developer ID, hardened runtime and a secure timestamp, and notarizes a temporary
zip containing those **same signed binaries** with `notarytool`. Accepted status
is required before creating tarballs. Tarballs cannot be stapled; the zip is only
a notarization submission, not a separate release asset. Temporary certificates,
keychains and API keys are removed even on failure. GPG signing uses nfpm's
embedded deb/rpm signatures, not an APT/Yum repository signing setup.

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
`release-assets` Actions artifact (seven-day retention). PR dry runs pass **no
platform signing secrets**. Fork PRs run lint only because their tokens cannot
write GitHub attestations; dispatch on a reviewed, trusted branch to test them.
The publishing workflow is never called by the PR dry run.

Credential-backed platform signing requires configured secrets and a trusted
nightly dispatch (with `dry_run: true`); no real release/tag is changed. Actual
upload-path testing requires maintainer approval and a disposable prerelease;
the PR validation must not publish to the upstream repo.
