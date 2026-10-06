# Contributing

## Language boundaries (mandatory)

- **TypeScript**: web dashboard and generated API clients only (`apps/web`).
- **Go**: control-plane, relay, mesh, scheduler, networking (`apps/*`, `go/`).
- **Rust**: local agent, storage, executors, game drivers (`apps/agent`,
  `crates/*`, `games/*`).

Cross-language contracts go through explicit schemas: Protobuf under
`proto/` (Rust↔Go IPC, mesh frames, relay) and OpenAPI under `api/openapi/`
(control-plane HTTP API). Regenerate with `make gen`; `make check-gen` fails
if committed generated files drift.

## Local development

See [docs/development.md](docs/development.md). Before pushing:

```sh
make fmt lint test build
```

## Commits

Small, focused commits; describe intent, not mechanics. Generated files are
committed alongside their schema changes.

## Developer Certificate of Origin

All contributions are accepted under the [Developer Certificate of
Origin](https://developercertificate.org/) (DCO). Sign off each commit
with `git commit -s`, which adds a `Signed-off-by` line certifying that
you wrote or otherwise have the right to submit the contribution under
the project's license (AGPL-3.0-or-later).
