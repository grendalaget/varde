# infra/cp — hosted control plane on Fly.io (LiteFS)

Reference deployment of the Varde control plane (`apps/controlplane`) at
`cp.varde.games` on Fly Machines with **LiteFS**: the sqlite db is replicated
to every machine, so reads are served locally everywhere and writes are
forwarded to the primary by LiteFS's built-in HTTP proxy. The image builds
straight from this checkout — deploy a tag by checking it out first.

## Prerequisites

- `flyctl` authenticated (`fly auth login`)
- A checkout of this repo; all commands run from the repo root.

## Deploy

```bash
fly apps create varde-cp                 # once

# LiteFS holds its lease in Fly's Consul — this sets FLY_CONSUL_URL.
fly consul attach

# One volume per machine, in the primary region. LiteFS keeps the
# replicated db + LTX files under /var/lib/litefs.
fly volumes create litefs -n 2 --region arn --size 1

fly deploy --config infra/cp/fly.toml    # builds infra/cp/Dockerfile, context=repo root

fly scale count 2 --region arn           # two write-capable machines

fly certs add cp.varde.games             # then point the DNS record at the app
```

## Scaling model

- **Writes**: only machines in `primary_region` (arn) are lease `candidate`s
  (`litefs.yml`). Run ≥2 there for write failover — Consul hands the lease
  to a survivor in ~10s; the LiteFS proxy holds writes briefly during the
  handoff (`primary-redirect-timeout`).
- **Reads**: every machine serves reads locally — `fly m clone --region lhr`
  (or any region) adds read replicas close to users; they never lead.
- **Capacity**: machines are **always-on** — LiteFS explicitly warns against
  combining autostop/autostart with leases (a stale machine can win the
  lease and discard newer writes). Scale the fleet with `fly scale count N`
  (arn, write-capable) or `fly m clone --region …` (elsewhere, read-only).
  `fly scale count` auto-provisions a `litefs` volume per new machine; each
  machine must have its own volume.
- The CP's reconciler runs only on the lease holder: `VARDE_PRIMARY_FILE`
  points at LiteFS's `/litefs/.primary` and `VARDE_NODE_NAME` (set by
  `varde-cp-entry`) names this node — see `litefs.yml` and
  `apps/controlplane/internal/store/elector.go`.

## Env

All non-secret config lives in `fly.toml [env]`: `VARDE_LISTEN` (app port,
fronted by the LiteFS proxy on 8080), `VARDE_DB` (sqlite inside the FUSE
mount), `VARDE_PUBLIC_URL`, `VARDE_SIGNUP`, `VARDE_PRIMARY_FILE`. Use
`fly secrets set` for anything sensitive in the future
(`VARDE_EMBEDDED_RELAY*` optional — needs a dedicated IPv4 + UDP service
block, not wired here).

## Operations

```bash
fly ssh console                        # into any machine
litefs export -name varde.db -o /tmp/varde.db   # consistent db snapshot
litefs db -name varde.db               # txid + replication position
```

Volumes are per-machine and durable — a destroyed machine's volume keeps its
copy; a *new* volume bootstraps from the cluster. Take periodic off-site
backups of the export above (LiteFS is pre-1.0; belt and suspenders).

## Known limits

- **Failover loss window**: a primary dying mid-transaction can drop
  uncommitted writes (replication lag). Small blast radius at this scale;
  acceptable for v1.
- Postgres mode remains fully supported (`VARDE_DB=postgres://…` + the
  advisory-lock elector) for deployments that outgrow LiteFS —
  e.g. write load too heavy for a single writer, or non-fly hosting.
- The embedded relay (UDP :3478) needs a dedicated IPv4 address and a
  `[[services]]` UDP block; left out until relays are actually needed.
