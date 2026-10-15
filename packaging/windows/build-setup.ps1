# Build VardeSetup-<version>.exe — the bespoke installer.
#
# Stages the payload into apps/setup/payload/, then builds varde-setup twice:
#   1. --no-default-features → payload\uninstall.exe (no payload embedded —
#      it's the uninstaller that ships inside the install dir)
#   2. default features      → target\release\varde-setup.exe carrying the
#      zip of everything in payload/, including uninstall.exe itself
#
# Usage:
#   packaging/windows/build-setup.ps1 -Version 0.2.0
#   (expects dist\varde-*.exe to already exist — in CI they're the signed
#   binaries; locally run the same build commands first)
param(
  [Parameter(Mandatory = $true)][string]$Version,
  [switch]$SkipUiBuild,
  [switch]$SkipPayloadBuild
)

$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
Set-Location $root

$payload = 'apps\setup\payload'
New-Item -ItemType Directory -Force $payload | Out-Null

if (-not $SkipUiBuild) {
  bun run --filter '@varde/setup-ui' build
  if ($LASTEXITCODE -ne 0) { throw 'setup ui build failed' }
}

# --- payload: the bits that land in Program Files\Varde ---------------------
if (-not $SkipPayloadBuild) {
  $need = @('varde-agent.exe', 'varde-mesh.exe', 'varde-tray.exe', 'varde-control-plane.exe')
  foreach ($n in $need) {
    if (-not (Test-Path "dist\$n")) { throw "missing dist\$n - build the release binaries first" }
    Copy-Item "dist\$n" $payload
  }
}
Copy-Item packaging\windows\varde.ico $payload
Copy-Item LICENSE $payload

# the WebView2 bootstrapper rides along; the installer runs it only when the
# runtime is missing (same evergreen link the NSIS bundle used)
$wv2 = Join-Path $payload 'MicrosoftEdgeWebview2Setup.exe'
if (-not (Test-Path $wv2)) {
  Invoke-WebRequest -Uri 'https://go.microsoft.com/fwlink/p/?LinkId=2124703' -OutFile $wv2
}

# lite build = uninstall.exe (compiles the same sources minus the payload zip)
cargo build --locked --release -p varde-setup --no-default-features
if ($LASTEXITCODE -ne 0) { throw 'cargo build (uninstall) failed' }
Copy-Item target\release\varde-setup.exe "$payload\uninstall.exe" -Force

# full build = the installer itself
cargo build --locked --release -p varde-setup
if ($LASTEXITCODE -ne 0) { throw 'cargo build (installer) failed' }

New-Item -ItemType Directory -Force release-assets | Out-Null
$out = "release-assets\VardeSetup-$Version.exe"
Copy-Item target\release\varde-setup.exe $out -Force
Write-Host "wrote $out"
