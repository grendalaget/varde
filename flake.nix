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
            bun
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
