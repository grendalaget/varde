#!/bin/sh
# Varde installer — https://varde.games
#
#   curl -fsSL https://github.com/grendalaget/varde/releases/latest/download/install.sh | sh
#
# Picks .deb (apt/dpkg) or .rpm (dnf/yum/rpm) when a package manager is
# present, and falls back to a tarball + systemd layout otherwise. Every
# download is checked against the release's SHA256SUMS before install.
#
# POSIX sh; works under dash, bash, busybox ash and in minimal containers.
set -eu

VARDE_REPO="${VARDE_REPO:-grendalaget/varde}"
VARDE_COMPONENT="${VARDE_COMPONENT:-agent}"
VARDE_VERSION="${VARDE_VERSION:-latest}"
VARDE_YES="${VARDE_YES:-}"
VARDE_UNINSTALL="${VARDE_UNINSTALL:-}"
VARDE_DRY_RUN="${VARDE_DRY_RUN:-}"
# Mirror/test override: VARDE_RELEASE_URL replaces the release-asset base
# (file:// works for local checks). Resolution runs through SHA256SUMS, so
# no GitHub API calls are needed and no rate limit applies.
VARDE_RELEASE_URL="${VARDE_RELEASE_URL:-}"

# ---------------------------------------------------------------- output

COLOR=''
if [ -z "${NO_COLOR:-}" ] && [ -z "${VARDE_NO_COLOR:-}" ] &&
  [ -t 1 ] && [ "${TERM:-dumb}" != "dumb" ]; then
  COLOR=1
fi

if [ -n "$COLOR" ]; then
  # Glød accent; mid-grey stones read on both light and dark terminals.
  GLOD=$(printf '\033[38;2;255;91;31m')
  STONE=$(printf '\033[38;5;245m')
  BOLD=$(printf '\033[1m')
  DIM=$(printf '\033[2m')
  RESET=$(printf '\033[0m')
else
  GLOD='' STONE='' BOLD='' DIM='' RESET=''
fi

say() { printf '%s\n' "$*"; }
step() { printf '%s\n' "  ${GLOD}›${RESET} $*"; }
ok() { printf '%s\n' "  ${GLOD}✓${RESET} $*"; }
warn() { printf '%s\n' "  ${BOLD}!${RESET} $*" >&2; }
fail() {
  printf '%s\n' "  ${BOLD}✗${RESET} $*" >&2
  exit 1
}

# The cairn rendered the way varde.games draws it on the landing page: the
# real mark geometry rasterized into the sparse glyph-field ramp
# (" ·:-~=+*xoasce%@"), lit stone in Glød — at a lower density for the
# terminal. Regenerate against brand/svg/mark paths if the mark changes.
banner() {
  cat <<EOF

                      ${STONE}==
                ${STONE}x o**+* o
               ${STONE}*xo**o o+ **

            ${STONE}=++xx+x* ${GLOD}~ox +*oxx        ${RESET}${BOLD}v a r d e${RESET}
            ${STONE}x+*oxx o  ${GLOD}*ox*+o++*       ${RESET}${DIM}Game servers built by many hands.${RESET}
           ${STONE}o+*oxx**o   ${GLOD}*o +**xx

         ${STONE}~** ++*xxo *x**o  x+x*o
         ${STONE}*oxooo xx  *o+***o** oxo
         ${STONE}++~~+~ ~+ =+~~~--~+=~**+
        ${STONE}+ *xx+x oxx++*x o + *  o*+
       ${STONE}+o ox+ox +* +ox  ox*xo++oo*~
      ${STONE}* x*o*+++*  o   *xx+***o+ oo
     ${STONE}~o ooo*o  * x+o +* x++ *oox
      ${STONE}+*--~${RESET}

EOF
}

usage() {
  cat <<EOF
Varde installer — install the agent, control plane or relay on Linux.

Usage: install.sh [OPTIONS]

Options:
  --component <name>   agent (default), control-plane or relay
  --version <ver>      latest (default), nightly, or a release like 1.2.3 / v1.2.3
  --yes                answer yes to prompts (non-interactive)
  --dry-run            resolve, download and verify only; do not install
  --uninstall          remove the installed component instead
  --help               show this help

Environment equivalents for piping:
  VARDE_COMPONENT, VARDE_VERSION, VARDE_YES=1, VARDE_UNINSTALL=1,
  VARDE_DRY_RUN=1, NO_COLOR=1

Examples:
  curl -fsSL .../install.sh | sh
  curl -fsSL .../install.sh | sh -s -- --component control-plane --yes
  curl -fsSL .../install.sh | VARDE_VERSION=nightly sh
EOF
}

# ------------------------------------------------------------- helpers

have() { command -v "$1" >/dev/null 2>&1; }

# fetch URL DEST — file:// is a plain copy (local mirrors, testing);
# otherwise curl or wget, with a progress bar only on a terminal.
fetch() {
  case "$1" in
  file://*)
    cp -f "${1#file://}" "$2"
    return
    ;;
  esac
  if have curl; then
    if [ -t 1 ]; then
      curl -fL --progress-bar --retry 3 -o "$2" "$1"
    else
      curl -fsSL --retry 3 -o "$2" "$1"
    fi
  elif have wget; then
    if [ -t 1 ]; then
      wget --show-progress -q -O "$2" "$1"
    else
      wget -q -O "$2" "$1"
    fi
  else
    fail "need curl or wget to download release assets"
  fi
}

sha256_of() {
  if have sha256sum; then
    sha256sum "$1" | awk '{print $1}'
  elif have shasum; then
    shasum -a 256 "$1" | awk '{print $1}'
  elif have openssl; then
    openssl dgst -sha256 "$1" | awk '{print $NF}'
  else
    fail "need sha256sum, shasum or openssl to verify downloads"
  fi
}

truthy() { [ "$1" = "1" ] || [ "$1" = "yes" ] || [ "$1" = "true" ]; }

# confirm MSG DEFAULT — prompt on the controlling terminal. DEFAULT is the
# answer for a bare Enter, and for non-interactive shells without --yes.
confirm() {
  if truthy "$VARDE_YES"; then return 0; fi
  if [ ! -r /dev/tty ]; then
    [ "$2" = "y" ]
    return
  fi
  printf '  %s ' "$1" >/dev/tty
  IFS= read -r answer </dev/tty || answer=''
  case "$answer" in
  '') [ "$2" = "y" ] ;;
  y | Y | yes | Yes | YES) return 0 ;;
  *) return 1 ;;
  esac
}

SUDO=''
need_root() {
  [ "$(id -u)" = "0" ] && return 0
  if have sudo; then
    SUDO=sudo
  elif have doas; then
    SUDO=doas
  else
    fail "root privileges required: re-run as root or install sudo"
  fi
  say ""
  say "  Root privileges are needed to:"
  printf '    %s\n' "$@"
  step "elevating with ${SUDO} — your password may be requested"
  $SUDO -v || fail "could not elevate privileges"
}

systemd_live() { [ -d /run/systemd/system ]; }

service_of() {
  case "$1" in
  agent) printf 'varde-agent.service varde-update.path' ;;
  *) printf 'varde-%s.service' "$1" ;;
  esac
}

# ----------------------------------------------------------------- args

while [ $# -gt 0 ]; do
  case "$1" in
  --component)
    [ $# -ge 2 ] || fail "--component needs a value"
    VARDE_COMPONENT="$2"
    shift 2
    ;;
  --component=*)
    VARDE_COMPONENT="${1#*=}"
    shift
    ;;
  --version)
    [ $# -ge 2 ] || fail "--version needs a value"
    VARDE_VERSION="$2"
    shift 2
    ;;
  --version=*)
    VARDE_VERSION="${1#*=}"
    shift
    ;;
  --yes | -y)
    VARDE_YES=1
    shift
    ;;
  --dry-run)
    VARDE_DRY_RUN=1
    shift
    ;;
  --uninstall)
    VARDE_UNINSTALL=1
    shift
    ;;
  --help | -h)
    banner
    usage
    exit 0
    ;;
  *) fail "unknown option: $1 (see --help)" ;;
  esac
done

case "$VARDE_COMPONENT" in
agent | control-plane | relay) ;;
*) fail "unknown component '$VARDE_COMPONENT' (agent, control-plane or relay)" ;;
esac
case "$VARDE_VERSION" in
'' | *[!-A-Za-z0-9._+]*) fail "invalid version '$VARDE_VERSION'" ;;
esac

PKG="varde-$VARDE_COMPONENT"

# ------------------------------------------------------------- platform

os=$(uname -s)
[ "$os" = "Linux" ] ||
  fail "this installer is for Linux; download the macOS/Windows assets from the release page"

machine=$(uname -m)
case "$machine" in
x86_64 | amd64) ARCH=amd64 ;;
aarch64 | arm64) ARCH=arm64 ;;
*) fail "unsupported architecture '$machine' — releases ship linux/amd64 and linux/arm64" ;;
esac

# ----------------------------------------------------------- uninstall

if truthy "$VARDE_UNINSTALL"; then
  banner
  manifest="/usr/lib/varde/tarball-$VARDE_COMPONENT.manifest"
  method=''
  if have dpkg && dpkg -s "$PKG" >/dev/null 2>&1; then
    method=deb
  elif have rpm && rpm -q "$PKG" >/dev/null 2>&1; then
    method=rpm
  elif [ -f "$manifest" ]; then
    method=tarball
  else
    fail "$PKG does not look installed (checked dpkg, rpm and $manifest)"
  fi
  say "  Removing ${BOLD}$PKG${RESET} ($method install)."
  confirm "Proceed? [y/N]" n || fail "aborted"
  case "$method" in
  deb)
    need_root "remove $PKG via the package manager"
    if have apt-get; then
      $SUDO apt-get remove -y "$PKG"
    else
      $SUDO dpkg -r "$PKG"
    fi
    ;;
  rpm)
    need_root "remove $PKG via the package manager"
    if have dnf; then
      $SUDO dnf remove -y "$PKG"
    elif have yum; then
      $SUDO yum remove -y "$PKG"
    else
      $SUDO rpm -e "$PKG"
    fi
    ;;
  tarball)
    need_root "stop the service and remove the installed files"
    if systemd_live; then
      for svc in $(service_of "$VARDE_COMPONENT"); do
        $SUDO systemctl disable --now "$svc" >/dev/null 2>&1 || true
      done
    fi
    # Deepest paths first so emptied directories can be removed.
    sort -r "$manifest" | while IFS= read -r path; do
      if [ -d "$path" ] && [ ! -L "$path" ]; then
        $SUDO rmdir "$path" 2>/dev/null || true
      else
        $SUDO rm -f "$path"
      fi
    done
    $SUDO rm -f "$manifest"
    ;;
  esac
  ok "$PKG removed"
  case "$VARDE_COMPONENT" in
  agent) say "  ${DIM}State in /var/lib/varde and config in /etc/varde were left behind.${RESET}" ;;
  control-plane) say "  ${DIM}State in /var/lib/varde-cp and config in /etc/varde were left behind.${RESET}" ;;
  relay) say "  ${DIM}Config in /etc/varde/relay.env was left behind.${RESET}" ;;
  esac
  exit 0
fi

# -------------------------------------------------------------- resolve

banner
step "detecting platform"
say "      ${DIM}linux/$ARCH · ${BOLD}$VARDE_COMPONENT${RESET}"

# The Go components are static, but the Rust agent links glibc.
if [ "$VARDE_COMPONENT" = "agent" ] && have ldd &&
  ldd --version 2>&1 | grep -qi musl; then
  warn "this system's libc is musl; the agent binary needs glibc — the control-plane and relay binaries are static and work"
fi

# A real package manager wins; otherwise the tarball path.
if have dpkg; then
  FORMAT=deb
elif have dnf || have yum || have rpm; then
  FORMAT=rpm
elif have tar; then
  FORMAT=tarball
else
  fail "no dpkg, rpm or tar found — cannot install on this system"
fi

# Map --version to a release-asset base URL. "latest" uses the
# releases/latest/download alias — no GitHub API call, so no rate limit.
case "$VARDE_VERSION" in
latest) base="https://github.com/$VARDE_REPO/releases/latest/download" ;;
nightly) base="https://github.com/$VARDE_REPO/releases/download/nightly" ;;
v*) base="https://github.com/$VARDE_REPO/releases/download/$VARDE_VERSION" ;;
*) base="https://github.com/$VARDE_REPO/releases/download/v$VARDE_VERSION" ;;
esac
[ -n "$VARDE_RELEASE_URL" ] && base="$VARDE_RELEASE_URL"

ext=$FORMAT
[ "$FORMAT" = "tarball" ] && ext=tar.gz

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT INT TERM

# SHA256SUMS is the single source of truth: it carries both the exact asset
# filename (with the embedded version, e.g. a nightly's +sha suffix) and the
# expected hash. One download resolves version and verification together.
step "resolving $VARDE_VERSION release"
fetch "$base/SHA256SUMS" "$WORK/SHA256SUMS" ||
  fail "SHA256SUMS not found under $base — the release may not exist or still be building"

# varde-<component>-<version>-linux-<arch>.<ext>; a pinned --version is
# matched literally (a nightly's "+sha" would misread as a regex), while
# latest/nightly pick up whatever filename the release ships.
case "$VARDE_VERSION" in
latest | nightly)
  re="^varde-${VARDE_COMPONENT}-[0-9A-Za-z.+-]*-linux-${ARCH}[.]${ext}\$"
  file=$(awk -v re="$re" '$NF ~ re {print $NF; exit}' "$WORK/SHA256SUMS")
  ;;
*)
  v=${VARDE_VERSION#v}
  file=$(awk -v f="varde-$VARDE_COMPONENT-$v-linux-$ARCH.$ext" \
    '$NF == f {print $NF; exit}' "$WORK/SHA256SUMS")
  ;;
esac
[ -n "$file" ] ||
  fail "no $VARDE_COMPONENT linux/$ARCH $ext asset listed in SHA256SUMS for $VARDE_VERSION"
expected=$(awk -v f="$file" '$NF == f {print $1; exit}' "$WORK/SHA256SUMS")

# Filename back to version for the plan line.
VERSION=${file#varde-"$VARDE_COMPONENT"-}
VERSION=${VERSION%-linux-"$ARCH"."$ext"}

say ""
say "  ${BOLD}plan${RESET}"
say "    component  $PKG"
say "    version    $VERSION ($VARDE_VERSION)"
say "    platform   linux/$ARCH"
say "    method     $FORMAT"
say "    asset      $file"
say ""
confirm "Install? [Y/n]" y || fail "aborted"

# ------------------------------------------------------- download+verify

step "downloading $file"
fetch "$base/$file" "$WORK/$file" ||
  fail "asset $file failed to download from $base"

actual=$(sha256_of "$WORK/$file")
[ "$actual" = "$expected" ] || fail "checksum mismatch for $file — refusing to install"
ok "SHA256 verified against the release's SHA256SUMS"

if truthy "$VARDE_DRY_RUN"; then
  say ""
  ok "dry run — would install $file via the $FORMAT path"
  case "$FORMAT" in
  deb) say "      ${DIM}apt-get install ./$file (dpkg -i fallback)${RESET}" ;;
  rpm) say "      ${DIM}dnf/yum install ./$file (rpm -U fallback)${RESET}" ;;
  tarball) say "      ${DIM}extract to /, install systemd units, enable the service${RESET}" ;;
  esac
  exit 0
fi

# -------------------------------------------------------------- install

say ""
case "$FORMAT" in
deb)
  need_root "install $file via the package manager (binary, systemd units, icons)"
  if have apt-get; then
    step "apt-get install $file"
    $SUDO env DEBIAN_FRONTEND=noninteractive apt-get install -y "$WORK/$file"
  else
    step "dpkg -i $file"
    $SUDO dpkg -i "$WORK/$file" ||
      warn "dpkg reported errors — run 'sudo apt-get install -f' if dependencies are missing"
  fi
  ;;
rpm)
  need_root "install $file via the package manager (binary, systemd units, icons)"
  if have dnf; then
    step "dnf install $file"
    $SUDO dnf install -y "$WORK/$file"
  elif have yum; then
    step "yum install $file"
    $SUDO yum install -y "$WORK/$file"
  else
    step "rpm -U $file"
    $SUDO rpm -U "$WORK/$file" ||
      warn "rpm reported errors — check for missing dependencies"
  fi
  ;;
tarball)
  need_root "extract $file to /, install systemd units and create the varde user"
  step "extracting $file"
  stage="$WORK/root"
  mkdir -p "$stage"
  # Extract as root so packaged modes survive into /; belt-and-suspenders
  # chown keeps root ownership even for tarballs built outside the pipeline.
  $SUDO tar -xzf "$WORK/$file" -C "$stage"
  $SUDO chown -R 0:0 "$stage"
  # /etc files are noreplace in the packages: keep an existing config.
  if [ -d "$stage/etc" ]; then
    (cd "$stage" && find ./etc -type f) | while IFS= read -r f; do
      rel=${f#./}
      if [ -e "/$rel" ]; then rm -f "$stage/$rel"; fi
    done
  fi
  (cd "$stage" && find . -mindepth 1 -print | sed 's|^\./|/|') >"$WORK/manifest"
  tar -C "$stage" -cf - . | $SUDO tar -xf - -C /
  $SUDO cp "$WORK/manifest" "/usr/lib/varde/tarball-$VARDE_COMPONENT.manifest"
  if [ "$VARDE_COMPONENT" = "agent" ] && ! getent passwd varde >/dev/null 2>&1; then
    if have useradd; then
      $SUDO useradd --system --home-dir /var/lib/varde --shell /usr/sbin/nologin varde || true
    else
      warn "no useradd — create a 'varde' system user or the service will not start"
    fi
  fi
  if systemd_live; then
    $SUDO systemctl daemon-reload || true
    for svc in $(service_of "$VARDE_COMPONENT"); do
      $SUDO systemctl enable --now "$svc" || true
    done
  fi
  ok "files recorded in /usr/lib/varde/tarball-$VARDE_COMPONENT.manifest"
  ;;
esac

# ---------------------------------------------------------- next steps

say ""
if ! systemd_live; then
  warn "no running systemd detected — the service was installed but not started"
  say "      ${DIM}start it under your init system, or run ${BOLD}$PKG${RESET}${DIM} directly${RESET}"
fi

case "$VARDE_COMPONENT" in
agent)
  ok "varde-agent installed"
  say ""
  say "  ${BOLD}next:${RESET} enroll this machine into your group"
  say "    sudo varde-agent enroll                 ${DIM}# device-code flow${RESET}"
  say "    sudo varde-agent enroll --token <token> ${DIM}# one-shot token${RESET}"
  say ""
  say "  Mint tokens in your control plane (https://cp.varde.games or a"
  say "  self-hosted one), then: systemctl status varde-agent"
  ;;
control-plane)
  ok "varde-control-plane installed"
  say ""
  say "  ${BOLD}next:${RESET} it listens on :8080 by default — browse http://localhost:8080"
  say "  Configure via /etc/varde/control-plane.env, then:"
  say "    systemctl status varde-control-plane"
  ;;
relay)
  ok "varde-relay installed"
  say ""
  say "  ${BOLD}next:${RESET} tune /etc/varde/relay.env if needed, then:"
  say "    systemctl status varde-relay"
  ;;
esac
say ""
