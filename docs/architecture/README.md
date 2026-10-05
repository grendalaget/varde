# Architecture

> A logical game server belongs to the group, not to whichever PC happens to run it.

varde is a substrate for running ordinary dedicated game servers on a group's own machines. Exactly one machine
runs a given server at a time; its persistent state is captured as immutable, content-addressed snapshots that are
replicated to other machines; a coordinator hands out time-bounded execution leases so that another machine can take
over when the host disappears; and a peer-to-peer mesh gives every player a stable local address for the server no
matter who hosts it.

This directory is the contract between components. Read in order:

1. [`README.md`](README.md) – components, boundaries, glossary (this file)
2. [`control-plane.md`](control-plane.md) – data model, public & agent APIs, leases/epochs, scheduler, reconciler
3. [`storage.md`](storage.md) – snapshot format, replication, anchors, retention
4. [`networking.md`](networking.md) – mesh, relay, service addresses, forwarding
5. [`agent.md`](agent.md) – node agent, executors, game drivers, fencing, local layout

## Components

| Component | Language | Binary | Runs on |
|---|---|---|---|
| Control plane | Go | `varde-control-plane` | one place: a VPS, a home server, a hosting provider. Embeds the web UI and (optionally) a relay |
| Relay | Go | `varde-relay` | anywhere with a public UDP port; optional when direct paths work |
| Mesh node | Go | `varde-mesh` | every machine, as a child process of the agent |
| Agent | Rust | `varde-agent` | every machine (Windows service / systemd unit) |
| Web dashboard | TypeScript | static SPA served by the control plane | browser |

```
           browser ──HTTPS(JSON, OpenAPI)──▶ control plane ◀──HTTPS(JSON, signed)── agent (every node)
                                                 │                                    │ gRPC over UDS / named pipe
                                                 │ relay tokens                       ▼
                                               relay ◀────── QUIC-in-UDP (opaque) ── mesh ◀═ QUIC (direct/punched) ═▶ mesh (peer)
```

### Hard boundaries

* **The dashboard only talks to the control-plane API.** Never to agents or peers.
* **The control plane coordinates; it never stores live worlds** and never relays game traffic. It cannot run
  arbitrary commands on nodes: every directive is a structured, validated operation (I9).
* **The agent decides *what*, the mesh decides *how*.** The agent tells the mesh which peers exist, which service
  routes to expose and which services it hosts (declaratively, full-replacement `Set*` RPCs, see
  `proto/mesh/v1/ipc.proto`). The mesh chooses direct / hole-punched / relayed paths and moves bytes.
* **Games are adapters.** Core code (control plane, agent core, mesh) has no game-specific logic. Game behaviour lives in
  driver crates under `games/` behind `crates/game-driver-api`; the control plane only knows a data-only catalog
  (ids, display names, ports, config schema, resource hints).

### Anchors (optional always-on replicas)

Any node can be marked **anchor**: an always-on machine (NAS, Proxmox VM, VPS, a hosting provider's storage box) whose
job is to *always hold the latest save*. Anchors are first in line for every replication, can be required for a
snapshot to count as safe (`require_anchor_for_commit`), and receive the final snapshot when a host shuts down
cleanly. An anchor may also host games if hosting is enabled, but needn't. Nothing requires an anchor: without one the
group works peer-to-peer, and the UI says plainly when the latest save exists on only one machine. See
[`storage.md`](storage.md#anchors).

### Self-hosted vs. hosted

The same binaries serve both audiences:

* **Friends:** one `varde-control-plane` (SQLite, embedded relay, embedded UI) on any always-on box, or a public
  instance run by someone else. The box running it can also run an agent as an anchor.
* **Hosting business:** control plane on PostgreSQL behind a load balancer, separate regional relays, provider-owned
  anchor/host nodes enrolled into customer groups with enrollment tokens, per-group limits (`group_limits`) and an
  operator-only admin API. Billing is out of scope but groups carry a `plan` string and usage counters to build on.

## Invariants (spec §63)

| | Invariant | Enforced by |
|---|---|---|
| I1 | ≤ 1 valid execution epoch per server | unique partial index on active executions + monotonic `servers.epoch`; new lease only after old expired/released |
| I2 | No lease ⇒ no hosting | agent self-fencing deadline computed conservatively from its own clock; CP omits stale executions from directives |
| I3 | Stale epochs never publish canonical state | every agent write carries `(server_id, execution_id, epoch, node_id)`; CP rejects mismatches with 409 `stale_epoch` |
| I4 | Automatic recovery uses committed snapshots only | scheduler's recovery path filters `state = committed` |
| I5 | Manifests immutable | snapshot id derived from manifest BLAKE3 digest |
| I6 | Chunks verified before use | BLAKE3 verified on every read and on every received chunk |
| I7 | Host change ≠ identity change | per-service stable loopback IP, routes keyed by `service_id` |
| I8 | CP loss never corrupts stored state | agents only delete snapshots on explicit CP instruction; restore writes to staging then renames |
| I9 | No remote shell | directives are typed; drivers build argv; no shell is ever invoked |
| I10 | Clean Windows install needs no config | installer + device-code linking (packaging/windows) |

## Glossary (engineering → user-facing, spec §41)

| Engineering | Dashboard |
|---|---|
| execution lease / holder | "Hosting on Arne-PC" |
| replica (ready) | "Backed up on" |
| latest committed snapshot | "Latest safe save: 18 s ago" |
| migration | "Moving server" |
| recovery after lease expiry | "Recovering server" |
| path kind | "Connection: Direct / Relayed" |
| anchor | "Always-on backup" |

The dashboard has an **Advanced** toggle that shows epochs, execution ids and snapshot digests.

## Identifiers

`<prefix>_<20 lowercase base32 chars>` (random, 100 bits) for `usr_ grp_ node_ srv_ dep_ exec_ svc_ inv_`.
Snapshot ids are derived: `snap_` + lowercase base32 (no padding) of the first 16 bytes of the manifest BLAKE3 digest.
Enrollment tokens: `vde_` + 32 base32 chars (secret; only a SHA-256 hash is stored). Device user codes: `WORD-NN-WORD`
from a fixed word list (case-insensitive).
