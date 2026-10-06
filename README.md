<h1 align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="brand/svg/varde-logo-dark.svg" />
    <img src="brand/svg/varde-logo.svg" alt="Varde" width="280" />
  </picture>
</h1>

<p align="center">Game servers built by many hands.</p>

Peer-hosted game servers for groups of friends. One active host at a time,
replicated snapshots, automatic failover, stable loopback service addresses,
and a P2P QUIC mesh with relay fallback — no router configuration required.

## Components

| Path                 | Language   | Purpose                                             |
| -------------------- | ---------- | --------------------------------------------------- |
| `apps/controlplane` | Go         | HTTP API, auth, membership, scheduler, rendezvous   |
| `apps/relay`         | Go         | Encrypted packet forwarding only (no game TLS)      |
| `apps/mesh`          | Go         | Per-node QUIC mesh daemon, spawned by the agent     |
| `apps/agent`         | Rust       | Local node agent: supervision, storage, executors   |
| `apps/web`           | TypeScript | Web dashboard (React + Vite)                        |
| `apps/desktop`       | Rust       | Windows tray (Tauri 2): link this PC, status        |
| `go/`                | Go         | Shared libraries (identity, ids, generated protos)  |
| `crates/`            | Rust       | Shared libraries (driver/executor APIs, storage)    |
| `games/`             | Rust       | Game drivers (minecraft, valheim, testgame)         |
| `proto/`             | Protobuf   | Wire contracts (mesh IPC, frames, relay)            |
| `api/openapi`        | OpenAPI    | Control-plane HTTP API contract                     |
| `packaging/`         | -          | Linux (nfpm/systemd) and Windows packaging          |

## Development

See [docs/development.md](docs/development.md) for toolchain setup, then:

```sh
make gen    # regenerate protobuf + OpenAPI clients
make build  # build everything
make test   # run all tests
make lint   # run all linters
```

## Releases

See [docs/releasing.md](docs/releasing.md) for release/nightly builds, assets,
signing setup and download verification.

## License

MIT — see [LICENSE](LICENSE).
