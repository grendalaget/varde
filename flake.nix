{
  description = "Varde development shell — Rust agent, Go control plane/mesh/relay, bun web toolchain";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";
    rust-overlay = {
      url = "github:oxalica/rust-overlay";
      inputs.nixpkgs.follows = "nixpkgs";
    };
  };

  outputs = { nixpkgs, flake-utils, rust-overlay, ... }:
    flake-utils.lib.eachDefaultSystem (system:
      let
        pkgs = import nixpkgs {
          inherit system;
          overlays = [ rust-overlay.overlays.default ];
        };

        # Stable Rust with the cross targets the repo exercises:
        #   x86_64-pc-windows-gnu  — windows cross-check (docs/development.md)
        #   aarch64-linux-android  — android-check job (std only; `cross` still
        #                            uses its docker image for the NDK)
        rustToolchain = pkgs.rust-bin.stable.latest.default.override {
          extensions = [ "clippy" "rustfmt" "rust-analyzer" "rust-src" ];
          targets = [ "x86_64-pc-windows-gnu" "aarch64-linux-android" ];
        };

        # varde-tray / apps/setup are workspace members, so
        # `cargo build --workspace` needs the Tauri 2 webview stack on Linux.
        # macOS and Windows use their native webviews and need nothing extra.
        tauriDeps = with pkgs; lib.optionals stdenv.hostPlatform.isLinux [
          pkg-config
          webkitgtk_4_1
          gtk3
          libsoup_3
          glib-networking
          libayatana-appindicator
          librsvg
          openssl
        ];

        # Linker for `cargo clippy --target x86_64-pc-windows-gnu`
        # (x86_64-w64-mingw32-gcc on PATH).
        mingwLinker = pkgs.lib.optionals pkgs.stdenv.hostPlatform.isLinux [
          pkgs.pkgsCross.mingwW64.buildPackages.gcc
        ];

        # bun is pinned to the project's release-asset version rather than
        # floating with nixpkgs (upstream assets, same fetch scheme as the
        # nixpkgs package).
        bunVersion = "1.4.1";
        bunAsset = {
          x86_64-linux = "bun-linux-x64-baseline";
          aarch64-linux = "bun-linux-aarch64";
          aarch64-darwin = "bun-darwin-aarch64";
          x86_64-darwin = "bun-darwin-x64-baseline";
        }.${system} or (throw "bun: unsupported system ${system}");
        bunHash = {
          x86_64-linux = "sha256-qMnGc4IC4vztVV3YYKlTxWwM0Fn3UEHnAQroGjKAJkY=";
          aarch64-linux = "sha256-WAzndTMQjcaxC+wXITl+T1qkTpCXJtokUdSD38XlgdY=";
          aarch64-darwin = "sha256-2Jc86DX6eGflzHmv7m/G8a4BF6pL1fwlRv0AxRL3E4Y=";
          x86_64-darwin = "sha256-SY521hu+h9Iwb2X+1groa2sMjuLacJ+DICgI2xoJ5Ac=";
        }.${system};
        bunPinned = pkgs.bun.overrideAttrs (old: {
          version = bunVersion;
          src = pkgs.fetchurl {
            url = "https://github.com/oven-sh/bun/releases/download/bun-v${bunVersion}/${bunAsset}.zip";
            hash = bunHash;
          };
          sourceRoot = bunAsset;
        });
      in
      {
        devShells.default = pkgs.mkShell {
          packages = with pkgs; [
            rustToolchain

            # control plane, relay, mesh, e2e
            go
            golangci-lint
            gopls

            # contracts: buf lint + buf.gen.yaml local plugins; protoc for
            # ad-hoc use (tonic-build uses protoc-bin-vendored at build time)
            buf
            protobuf
            protoc-gen-go
            protoc-gen-go-grpc
            oapi-codegen

            # apps/web, apps/desktop/ui, apps/setup/ui, tests/minecraft-bot
            bunPinned
            nodejs_22

            # `make package` -> .deb/.rpm
            nfpm

            git
          ] ++ tauriDeps ++ mingwLinker;

          shellHook = ''
            echo "varde dev shell — $(rustc --version | cut -d' ' -f1-2), $(go version | awk '{print $3}'), bun $(bun --version)"
          '';
        };
      });
}
