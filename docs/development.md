# Development setup

Toolchain install commands used on Ubuntu 22.04 (adjust paths for other
platforms):

```sh
# Go 1.27.x (latest stable)
curl -sL https://go.dev/VERSION?m=text            # check latest
curl -sL -o /tmp/go.tgz https://go.dev/dl/go1.27.1.linux-amd64.tar.gz
sudo tar -C /usr/local -xzf /tmp/go.tgz
export PATH=/usr/local/go/bin:$HOME/go/bin:$PATH

# Node.js 22 LTS + pnpm (npm-installed; corepack also works)
curl -sL -o /tmp/node.tar.xz https://nodejs.org/dist/v22.20.0/node-v22.20.0-linux-x64.tar.xz
sudo tar -C /usr/local --strip-components=1 -xJf /tmp/node.tar.xz
sudo npm install -g pnpm

# buf (protobuf lint + generation)
sudo install -m 0755 \
  <(curl -sL https://github.com/bufbuild/buf/releases/latest/download/buf-Linux-x86_64) \
  /usr/local/bin/buf

# Go plugins + tools
go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest
go install github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@latest
go install github.com/goreleaser/nfpm/v2/cmd/nfpm@latest

# golangci-lint
curl -sL -o /tmp/gl.tar.gz \
  https://github.com/golangci/golangci-lint/releases/latest/download/golangci-lint-<ver>-linux-amd64.tar.gz
sudo tar -C /usr/local/bin --strip-components=1 -xzf /tmp/gl.tar.gz \
  --wildcards '*/golangci-lint'

# Rust (rustup) + Windows cross-check target + mingw-w64 linker
curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs | sh -s -- -y
rustup target add x86_64-pc-windows-gnu
sudo apt-get install -y gcc-mingw-w64-x86-64
```

Verified versions on this machine (2026-10): Go 1.27.1, Node 22.20.0, pnpm
12.9.1, buf 1.73.0, golangci-lint 2.14.0, rustc 1.97.1, nfpm 2.47.0,
oapi-codegen 2.8.0, mingw-w64 gcc 10.

## Workflow

```sh
make gen        # buf generate (Go protos) + openapi-typescript (web client)
make build      # go build all modules + cargo build + pnpm build
make test       # go test + cargo test
make lint       # buf lint, go vet, golangci-lint, clippy, eslint
make fmt        # gofmt, cargo fmt, prettier
make check-gen  # fail if generated files drift from the schemas
```

Language boundaries (mandatory, spec §3): TypeScript only for the web
dashboard; Go for control-plane, relay and mesh; Rust for the agent,
storage, executors and game drivers.

## Cross-checking the Windows build

```sh
cargo check --target x86_64-pc-windows-gnu -p p2pgames-agent
(cd apps/mesh && GOOS=windows go build ./...)
```
