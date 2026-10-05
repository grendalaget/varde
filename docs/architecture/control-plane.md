# Control plane

Single Go binary `varde-control-plane serve`. Modules (packages under `apps/control-plane/internal/`):
`store` (SQL), `auth`, `api` (public, OpenAPI-generated server), `agentapi`, `scheduler` (pure, deterministic),
`reconciler` (the only writer of lease/execution state transitions besides agent reports), `events` (durable log +
SSE fan-out), `relaytoken`, `catalog` (game catalog data), `webui` (embedded SPA), optional embedded relay.

## Storage

`database/sql` + `sqlx` with **portable SQL** and `sqlx.Rebind` for placeholders. Two drivers:

* `sqlite` (default, `modernc.org/sqlite`, no cgo): `--db sqlite:///var/lib/varde-cp/cp.db` (WAL, busy timeout,
  `_txlock=immediate`).
* `postgres` (`pgx/v5/stdlib`): `--db postgres://...`.

Portability rules: timestamps are `BIGINT` unix milliseconds; booleans are `INTEGER` 0/1; JSON is `TEXT`; ids are
`TEXT`; event ids use an integer sequence (`INTEGER PRIMARY KEY AUTOINCREMENT` in SQLite, `BIGSERIAL` in PG, the only
dialect-specific DDL, kept in per-dialect migration files). Migrations embedded, applied on start. All tests run on
SQLite; CI additionally runs the store tests against PostgreSQL.

### Tables

```
users(id, email UNIQUE, display_name, password_hash, is_operator, created_at)
sessions(token_hash PK, user_id, created_at, expires_at)
groups(id, name, plan, settings_json, created_at)
     settings: {snapshot_interval_s:120, default_replication_factor:3, default_min_commit_replicas:2,
                require_anchor_for_commit:false, snapshot_retention:10}
group_limits(group_id PK, max_nodes, max_servers, max_storage_bytes, max_relay_bytes_month)   -- NULL = unlimited
group_members(group_id, user_id, role{owner,admin,member}, created_at, PK(group_id,user_id))
invites(code PK, group_id, role, created_by, expires_at, max_uses, uses)
enrollment_tokens(id, group_id, token_hash UNIQUE, anchor, hosting_enabled, labels_json, expires_at, max_uses, uses, created_by)
device_links(user_code PK, device_code_hash UNIQUE, public_key, hostname, os, arch, agent_version,
             state{pending,approved,consumed,expired}, group_id, node_id, approved_by, expires_at, created_at)
nodes(id, group_id, name, public_key UNIQUE, os, arch, agent_version, hosting_enabled, anchor, priority,
      max_memory_mb, max_cpu_percent, max_storage_bytes, admin_state{active,draining,disabled},
      last_seen_at, created_at)
node_status(node_id PK, status_json, mesh_json, updated_at)   -- latest heartbeat payload (capabilities, endpoints, paths)
deployments(id, group_id, game_id, spec_json, digest, created_at)          -- immutable
servers(id, group_id, name, game_id, deployment_id, desired_state{running,stopped}, observed_state,
        replication_factor, min_commit_replicas, snapshot_interval_s, preferred_node_id, epoch BIGINT,
        config_json, created_at, updated_at)
services(id, server_id UNIQUE, group_id, loopback_ip, ports_json, UNIQUE(group_id, loopback_ip))
server_executions(id, server_id, node_id, epoch, state, action{run,stop}, stop_reason,
                  lease_expires_at, restore_snapshot_id, placement_json, health, message,
                  created_at, started_at, ended_at, end_reason{stopped,lost,failed,migrated,fenced})
     UNIQUE INDEX one_active ON server_executions(server_id) WHERE ended_at IS NULL     -- I1
migrations(id, server_id, from_execution_id, to_node_id, state, final_snapshot_id, created_at, finished_at)
snapshots(id, server_id, group_id, epoch, execution_id, node_id, parent_id, manifest_digest, deployment_id,
          reason{scheduled,manual,final,migration}, state{local,replicating,committed,superseded,invalid},
          size_bytes, stored_bytes, file_count, chunk_count, pinned, created_at, committed_at)
snapshot_replicas(snapshot_id, node_id, state{assigned,ready,deleting}, assigned_at, ready_at, PK(snapshot_id,node_id))
snapshot_requests(request_id PK, server_id, execution_id, reason, created_at, claimed_at)  -- queued one-off snapshot requests, delivered via directives
kv(key PK, value)                                                                          -- CP-internal state (e.g. relay signing key)
events(id SEQ, group_id, server_id, node_id, type, data_json, created_at)
audit_log(id SEQ, group_id, actor_type{user,node,system}, actor_id, action, target, data_json, created_at)
execution_logs(execution_id, seq, at, stream{stdout,stderr,agent}, line)   -- capped to last 2000 lines per execution
```

## Authentication

* **Users:** email + password (argon2id). `POST /v1/auth/login` sets `varde_session` (HttpOnly, SameSite=Lax, Secure
  when TLS) and also returns the token for API/CLI use as `Authorization: Bearer`. Signup policy
  `--signup=open|invite|closed` (default `open` until the first user exists, then `invite`). The very first user is
  `is_operator`. `auth.Provider` interface so OIDC can be added without touching handlers.
* **Nodes:** every `/v1/agent/*` call after enrollment carries
  `X-Varde-Node`, `X-Varde-Timestamp` (unix ms), `X-Varde-Signature` = base64(ed25519 over
  `"varde-agent-v1\n" + METHOD + "\n" + PATH + "\n" + TIMESTAMP + "\n" + hex(sha256(body))`). Skew > 60 s ⇒ 401.
  Disabled nodes ⇒ 403. No bearer tokens to leak; the private key never leaves the node.
* **Authorization** is enforced server-side per group role: `member` may start/stop/move servers and manage *own*
  nodes; `admin` additionally servers CRUD, invites, enrollment tokens, any node; `owner` group settings/deletion/roles.
  Operators may use `/v1/admin/*`.

## Enrollment

1. **Device code** (installer / desktop): agent `POST /v1/agent/enroll/device {public_key, hostname, os, arch,
   agent_version}` → `{device_code, user_code:"WOLF-73-KITE", verification_url:"<base>/link?code=WOLF-73-KITE",
   expires_in:900, interval:3}`. The installer opens `verification_url`; the signed-in user picks a group and approves
   (`POST /v1/device-links/approve {user_code, group_id, name}`). Agent polls `POST /v1/agent/enroll/device/poll
   {device_code}` → `202 pending` | `200 {node_id, group_id, control_plane_public_key}`.
2. **Enrollment token** (headless Linux, anchors, provider fleets): admin creates
   `POST /v1/groups/{id}/enrollment-tokens {anchor, hosting_enabled, expires_at, max_uses}`; on the node
   `varde-agent enroll --server URL --token vde_…` → `POST /v1/agent/enroll/token {token, public_key, ...}`.

Group limits (`max_nodes`) are checked at approval/enrollment.

## Agent protocol: heartbeat = reconciliation

Agents run a single loop: `POST /v1/agent/heartbeat` every `heartbeat_interval_ms` (default 5000). The request
reports observed state; the response is the **complete desired state for that node**. Both are idempotent, so a
restart of either side re-converges.

Request `AgentHeartbeat`:
```jsonc
{
  "agent_version": "0.1.0",
  "capabilities": {"os":"linux","arch":"x86_64","cpu_cores":8,"memory_total_mb":32000,"memory_available_mb":20000,
                    "disk_free_bytes":..., "on_battery":false, "user_active":false,
                    "runtimes":["native","java","steamcmd"], "cached_deployments":["dep_…"], "drivers":["testgame","minecraft"]},
  "mesh": {"listen_port":41641, "local_endpoints":["192.168.1.5:41641"], "observed_endpoints":["84.1.2.3:41641"],
           "relay_ids":["eu-1"], "peers":[{"node_id":"node_…","path":"direct","rtt_us":12000}]},
  "executions": [{"execution_id":"exec_…","server_id":"srv_…","epoch":7,"state":"running","health":"ok",
                  "message":"", "player_count": null}],
  "snapshots_stored_bytes": 123456
}
```
Response `AgentDirectives`:
```jsonc
{
  "server_time_unix_ms": 1790000000000,
  "heartbeat_interval_ms": 5000,
  "lease_ttl_ms": 20000,
  "node": {"name":"Arne-PC","hosting_enabled":true,"anchor":false,"admin_state":"active"},
  "executions": [{                       // leases this node holds; anything else it runs MUST stop (fencing)
     "execution_id":"exec_…","server_id":"srv_…","server_name":"Old World","epoch":7,
     "lease_expires_at_unix_ms": 1790000020000,
     "action":"run",                     // or "stop" (graceful: barrier → final snapshot → stop → report stopped)
     "stop_reason": null,                // "user" | "migration" | "drain"
     "deployment": {"id":"dep_…","game_id":"minecraft","spec":{...}},
     "config": {...},
     "service": {"service_id":"svc_…","loopback_ip":"127.77.12.34","ports":[{"port":25565,"protocol":"tcp"}]},
     "restore": {"snapshot_id":"snap_…","manifest_digest":"…","source_node_ids":["node_b","node_c"]},   // null = start without a save
     "snapshot_interval_s": 120,
     "snapshot_requests": [{"request_id":"…","reason":"manual"}]
  }],
  "peers": [{"node_id":"node_…","name":"Bob-PC","public_key":"<b64>","endpoints":["…"],"relay_ids":["eu-1"]}],
  "routes": [{"service_id":"svc_…","server_id":"srv_…","server_name":"Old World","loopback_ip":"127.77.12.34",
              "ports":[...],"host_node_id":"node_a","epoch":7}],          // all services in the group
  "relays": [{"relay_id":"eu-1","addr":"relay.example.com:3478","token":"…"}],
  "replication_tasks": [{"snapshot_id":"snap_…","server_id":"srv_…","manifest_digest":"…","source_node_ids":["node_a"]}],
  "delete_snapshots": ["snap_…"]          // explicit only (I8)
}
```

Lease renewal is implicit: for each reported execution that is still the server's active execution, held by this
node, with matching epoch and not `stopped/failed`, the CP extends `lease_expires_at = now + lease_ttl` **before**
writing the response. Executions the node reports but which are not in `executions` must be stopped immediately by the
agent (the CP has already moved on).

Immediate transitions use `POST /v1/agent/executions/{id}/status {server_id, epoch, state, health, message}` so the
UI does not wait for the next heartbeat. Mismatched `(server_id, epoch, node_id)` ⇒ 409 `stale_epoch`.

Other agent endpoints:
* `POST /v1/agent/snapshots {snapshot_id, server_id, execution_id, epoch, parent_id, manifest_digest, deployment_id,
  reason, size_bytes, stored_bytes, file_count, chunk_count, request_id?}` — accepted only if the execution is the active
  one, held by the caller, with matching epoch, **and its lease has not expired** (I3). Else 409 `stale_epoch`.
  Final snapshots (`reason=final|migration`) are accepted while the lease is still valid — heartbeats keep
  extending it during a `stop` in progress, so a final snapshot late in the stop is covered the same way.
* `POST /v1/agent/snapshots/{id}/replicas {state:"ready"}` — after the node verified every chunk.
* `POST /v1/agent/executions/{id}/logs {lines:[{at,stream,line}]}`.

### Execution states (reported by agent)
`preparing → restoring → starting → running → stopping → stopped` and `failed` (terminal: crash loop exhausted or
unrecoverable prepare/restore error, with `message`).

## Leases, epochs, fencing

* Activating a server = in **one transaction**: `UPDATE servers SET epoch = epoch + 1 WHERE id = ? AND epoch = ?`,
  insert `server_executions` row with the new epoch and `lease_expires_at = now + lease_ttl + start_grace`. The unique
  partial index makes a second concurrent activation fail (I1).
* A new execution is created only when the server has **no** active execution. An active execution ends only by
  (a) agent reporting `stopped`/`failed`, or (b) the reconciler observing `now > lease_expires_at` (`end_reason=lost`).
* **Agent fencing deadline** = `local_monotonic_at_request_send + (lease_expires_at − server_time) − fence_margin`
  (`fence_margin` default 5 s). Because the send time precedes the CP's receive time, the agent always stops *before*
  the CP considers the lease expired, without clock synchronisation. When the deadline passes the agent kills the game
  (graceful attempt bounded to `fence_margin/2`, then hard kill) and marks the execution `fenced` locally; it never
  uploads snapshots for it.
* Defaults (all configurable): heartbeat 5 s, node `suspect` after 15 s without heartbeat, `offline` after 30 s, lease
  TTL 20 s, start grace 600 s for `preparing/restoring` (downloads) — the grace only applies until the agent reports
  `running` or the first renewal.

## Reconciler

One goroutine, tick 1 s (and woken by API mutations / heartbeats), processes servers in id order. For each server:

| Condition | Action | Observed state |
|---|---|---|
| active exec, lease expired | end exec `lost`; emit `lease.expired` | `recovering` (if desired running) |
| desired running, no active exec | `scheduler.Place(recovery = previous exec lost)`; create exec | `starting` / `recovering` |
| desired running, placement impossible | emit `server.unschedulable` with reasons (rate-limited) | `failed` w/ message, retried every 15 s |
| desired stopped, active exec action=run | set `action=stop, stop_reason=user` | `stopping` |
| migration requested | old exec `action=stop, stop_reason=migration`; after its final snapshot is `ready` on target and old exec ended → new exec on target restoring from it | `migrating` |
| exec reports running+healthy | — | `running` |

Restore selection when activating:
* **Recovery (previous exec lost)**: newest `committed` snapshot that has a `ready` replica on an online node (I4).
* **Normal start**: newest non-invalid snapshot of the server. If none of its replicas are online, refuse with
  `409 latest_save_unavailable {snapshot_id, created_at, nodes}` unless the request sets `allow_older_snapshot:true`,
  in which case use the newest available. (This is the "Alice shut down her PC with the latest save" case — we never
  silently fork.)
* `source_node_ids` = online nodes with a `ready` replica; the chosen host gets the snapshot as `restore`.

## Scheduler (pure function)

`Place(server, nodes []NodeView, snapshots, mode) (Decision{node_id, score, reasons[]}, error)` — deterministic: same
input ⇒ same output; ties broken by `node_id`. Reasons are stored in `placement_json` and shown as "Why".

Hard constraints: node online, `admin_state=active`, `hosting_enabled`, driver available for `game_id`, OS/arch
supported by the game catalog entry, `memory_available ≥ required` and within `max_memory_mb`, disk free ≥ snapshot
size × 2 + deployment size, required runtime available.

Weights: restore snapshot already local +100 · deployment cached +30 · anchor (always-on) +80 · preferred node +60 ·
user priority (0–100) ×0.5 · uptime ≥ 24 h +20 · direct paths to ≥ half the online peers +20 · user active (gaming)
−40 · disk free < 10 % −50 · on battery −500.

## Events & SSE

Every state change appends to `events` in the same transaction: `node.enrolled node.online node.suspect node.offline
server.created server.start_requested server.stop_requested server.assigned server.started server.stopped
server.failed server.unschedulable lease.issued lease.expired execution.state snapshot.created
snapshot.replica_ready snapshot.committed snapshot.superseded migration.started migration.completed route.updated`.
`GET /v1/groups/{id}/events?after=<id>&limit=` and `GET /v1/groups/{id}/events/stream` (SSE, `id:` = event id,
honours `Last-Event-ID`, heartbeat comment every 15 s).

## Public API (OpenAPI: `api/openapi/control-plane.yaml`)

```
POST /v1/auth/signup | /v1/auth/login | /v1/auth/logout      GET /v1/me
GET|POST /v1/groups              GET|PATCH|DELETE /v1/groups/{groupId}
GET /v1/groups/{groupId}/members   PATCH|DELETE /v1/groups/{groupId}/members/{userId}
POST /v1/groups/{groupId}/invites  POST /v1/invites/{code}/accept   GET /v1/invites/{code}
GET|POST /v1/groups/{groupId}/enrollment-tokens   DELETE /v1/groups/{groupId}/enrollment-tokens/{tokenId}
GET /v1/device-links/{userCode}    POST /v1/device-links/approve
GET /v1/groups/{groupId}/nodes     GET|PATCH|DELETE /v1/nodes/{nodeId}
GET /v1/games
GET|POST /v1/groups/{groupId}/servers   GET|PATCH|DELETE /v1/servers/{serverId}
POST /v1/servers/{serverId}/start {preferred_node_id?, allow_older_snapshot?}
POST /v1/servers/{serverId}/stop
POST /v1/servers/{serverId}/move {target_node_id}
GET|POST /v1/servers/{serverId}/snapshots      PATCH /v1/snapshots/{snapshotId} {pinned}
GET /v1/servers/{serverId}/executions          GET /v1/servers/{serverId}/logs?execution_id=
GET /v1/groups/{groupId}/events                GET /v1/groups/{groupId}/events/stream
GET /v1/admin/groups                           PUT /v1/admin/groups/{groupId}/limits
GET /healthz  GET /readyz  GET /metrics  GET /v1/version  GET /v1/relay/public-key
```
Server views returned to the UI include a computed `summary`: `hosting_on {node_id,name}`, `latest_safe_save
{snapshot_id, created_at, replicas:[{node_id,name,anchor}]}`, `latest_save` (may be newer & not yet safe),
`only_on_one_machine: bool`, `address "127.77.12.34:25565"`, `connection` per node.

Agent endpoints (`/v1/agent/*`) are in the same OpenAPI file under tag `agent`, so the Rust agent's request/response
types can be checked against it.

## Relay tokens

The CP holds an ed25519 signing key (generated on first start, stored in the DB `kv` table). Token =
`base64url(json{node_id, pk (b64 node public key), group_id, relay_id, exp_unix_ms}) + "." + base64url(sig)`, TTL 1 h, re-issued in every
heartbeat. Relays are configured with the CP public key (`GET /v1/relay/public-key`) and forward only between nodes of
the same `group_id`. Relays are declared in CP config (`--relay id=eu-1,addr=relay.example.com:3478`); `--embedded-relay
:3478` runs one in-process for single-box self-hosting.
