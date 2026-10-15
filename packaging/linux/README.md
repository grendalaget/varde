# Varde on Linux

Packages are the native install UX on Linux. `install.sh` is the friendly
front door — a branded `curl | sh` installer that picks the right format,
verifies the download, and hands off to your package manager.

## Install

```sh
curl -fsSL https://github.com/grendalaget/varde/releases/latest/download/install.sh | sh
```

Default component is `agent`. Options (env-var equivalents work for piping):

| Flag | Env | Purpose |
| --- | --- | --- |
| `--component agent\|control-plane\|relay` | `VARDE_COMPONENT` | which component to install (default `agent`) |
| `--version latest\|nightly\|<ver>` | `VARDE_VERSION` | release to install (default `latest`; `1.2.3` or `v1.2.3` pin a tag) |
| `--yes` | `VARDE_YES=1` | answer yes to prompts — required when piping in automation |
| `--dry-run` | `VARDE_DRY_RUN=1` | resolve, download and verify only; install nothing |
| `--uninstall` | `VARDE_UNINSTALL=1` | remove the component instead |
| `--help` | — | usage |

`NO_COLOR` (or `VARDE_NO_COLOR`) disables color; non-tty output is always
plain. `VARDE_REPO` points at a fork, `VARDE_RELEASE_URL` at a mirror or a
local directory (`file://`) — useful for testing.

```sh
# control plane, non-interactive
curl -fsSL .../install.sh | sh -s -- --component control-plane --yes
# nightly agent
curl -fsSL .../install.sh | VARDE_VERSION=nightly sh
```

## What it does

1. Detects `linux/amd64` or `linux/arm64` and picks a format: `.deb` when
   `dpkg` exists, `.rpm` when `dnf`/`yum`/`rpm` does, else a root-filesystem
   `tar.gz` + systemd fallback.
2. Downloads the release's `SHA256SUMS` and resolves the exact asset name
   from it (the embedded version too — no GitHub API calls, so no rate
   limits), then downloads the asset and compares its SHA-256 before
   installing anything.
3. Explains which privileges it needs, elevates with `sudo` (or `doas`),
   and installs: `apt-get install` / `dnf`/`yum` / `rpm -U`, or for the
   tarball path, extraction to `/` with the packaged systemd units enabled
   and a file manifest at `/usr/lib/varde/tarball-<component>.manifest`
   for later `--uninstall`.

The packages themselves keep the existing behavior — the maintainer scripts
(`packaging/linux/scripts/`) create the `varde` user, enable and start the
systemd units, and `try-restart` on upgrades. With no running systemd
(e.g. containers) the installer prints the manual-start path instead.

### Formats and self-updates

- **deb / rpm**: fully supported. `varde-agent update` finds these installs
  by package-manager record and applies new versions via the release's
  `latest.json` feed.
- **snap**: strict-confinement sideloads, also released per component;
  notify-only for `varde-agent update` until store-published (snaps
  self-refresh via snapd then).
- **tarball**: distro-agnostic fallback; `varde-agent update` treats it as
  notify-only (no package-manager record to resolve), so re-run `install.sh`
  for upgrades. Not listed in `latest.json` by design.
- **install.sh itself** ships as a release asset (`install.sh`) covered by
  `SHA256SUMS`, minisign `.sig`, cosign `.sigstore.json` and provenance like
  every other payload. Verification beyond checksums: `docs/releasing.md`.

## AppStream / software-store metadata

The agent package ships `games.varde.agent.metainfo.xml` into
`/usr/share/metainfo/` so GNOME Software, KDE Discover and friends show a
proper listing: name, summary, the Glød `#FF5B1F` brand accent for light and
dark, links (varde.games, docs, source, issue tracker) and a dashboard
screenshot (`packaging/linux/screenshots/dashboard.png`, referenced by its
raw.githubusercontent URL). The `varde.desktop` launcher stays the
`launchable` — Name/Comment/Icon match the metainfo, so the store entry and
the app-menu entry are the same thing.

## Files

| Path | Purpose |
| --- | --- |
| `install.sh` | the `curl \| sh` installer (release asset `install.sh`) |
| `nfpm*.yaml` | deb/rpm package definitions per component |
| `games.varde.agent.metainfo.xml` | AppStream metadata for the agent |
| `varde*.service`, `varde-update.*`, `*.env` | systemd units + env files |
| `scripts/` | maintainer scripts run by the packages |
| `snap/` | snapcraft projects per component |
| `icons/` | hicolor icons for the desktop file / store listing |
| `screenshots/` | store-listing screenshots referenced by the metainfo |
