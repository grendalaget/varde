# Spike (c): Varde host-move timing

## Setup

Ran the experiment in the isolated detached worktree
`/home/ubuntu/spikes/varde-c` at `origin/main` commit `a1b154a`; the original
`/home/ubuntu/repos/varde` checkout was not built in or modified. The worktree
contains the local CP, mesh, agent, and testgame binaries plus
`run_spike_c.py`. Control plane and agent data, logs, and JSONL results are
under the same worktree.

The harness started one local CP and two agents using loopback prefixes
`127.77.` and `127.78.`, then ran `testgame` on TCP port 7777. CP timing flags
were left at defaults. The server explicitly used replication factor 2,
minimum commit replicas 2, and snapshot interval 3600 seconds; before each
abrupt-loss trial, the harness requested a manual snapshot and waited for it
to be committed and ready on the peer.

Planned moves used `POST /v1/servers/{serverId}/move` with
`{"target_node_id":"..."}`. `testgame` uses serial mode: the handler only
enables warm handoff for Minecraft, and otherwise marks the current execution
to stop (`apps/controlplane/internal/api/handlers_servers.go:51-56,497-550`).
The reconciler waits for the source execution to end, then for the final
snapshot's target replica to become ready, before activating the target
(`apps/controlplane/internal/reconciler/reconciler.go:623-685,691-708,803-842`).

Milestones were polled every 250 ms: source execution `ended_at` for “old
stopped,” `snapshot_replicas.state=ready` for “replication ready,” then a
running target execution plus a successful testgame `GET` for “new host
healthy.” A concurrent TCP client sent `GET` through the former host's stable
loopback service address every 250 ms. Its downtime is the interval from the
last successful sample before the move to the first successful sample after
the first failure; measurements therefore have approximately 250 ms sampling
resolution.

## Planned moves

Times are elapsed from the move request. Phase columns are incremental.

| Trial | Request → source stopped | Source stopped → replica ready | Replica ready → healthy | Total → healthy | Stable-route downtime |
|---|---:|---:|---:|---:|---:|
| 1 | 9.62 s | 5.06 s | 5.07 s | 19.75 s | 19.75 s |
| 2 | 9.62 s | 5.07 s | 5.07 s | 19.75 s | 19.75 s |
| 3 | 9.61 s | 5.06 s | 5.07 s | 19.73 s | 19.75 s |

All three migrations completed in `done` state. The former host's stable
address reached the new serving execution after each move; the measured
interruption was essentially the full serial-move duration.

## Abrupt host loss

For each trial, the active agent and its descendants were sent `SIGKILL`
(including its mesh and testgame processes). Recovery time starts at the kill
and ends when the peer had a running execution and answered testgame `GET`.
Because the peer already had a committed, ready replica before the kill, the
measurement covers lease expiry/reconciliation and the subsequent restore and
startup, not waiting for a new replica to be copied after failure.

| Trial | Killed host | Recovery to healthy peer |
|---|---|---:|
| 1 | `node_z77fqfhioipgtldzbgsk` | 24.95 s |
| 2 | `node_uvbmav653nvsb5dwihh3` | 25.21 s |
| 3 | `node_z77fqfhioipgtldzbgsk` | 19.92 s |

Mean: **23.36 s**; median: **24.95 s**; range: **19.92–25.21 s**. All three
snapshots were committed and replicated to the recovery peer before the kill.

## Stable-forward result and topology limitation

The existing mesh on the former host continued to expose its stable local
service address and automatically reached the new host after each planned
move. The corrected 250 ms probe measured **19.75 s** of downtime in all three
trials. This confirms re-pointing for the former host's mesh after it becomes
non-hosting. With only the requested two agents, there was no third,
independent non-hosting observer; this does not establish behavior from a
third member's forward.

## Timing knobs and code references

- CP defaults in effect were heartbeat interval **5,000 ms**, lease TTL
  **20,000 ms**, start grace **30,000 ms**, suspect threshold **15,000 ms**,
  and offline threshold **30,000 ms** (`apps/controlplane/main.go:132-136`,
  copied into reconciler timings at `main.go:189-194`). The reconciler tick
  defaults to **1 second** (`apps/controlplane/internal/reconciler/reconciler.go:36-45,77-96`).
- A running execution's heartbeat renews its lease to `now + LeaseTTLMs`
  (`apps/controlplane/internal/api/handlers_agent.go:191-216`). The reconciler
  ends an active execution as `lost` when that lease expires, then proceeds
  with recovery (`apps/controlplane/internal/reconciler/reconciler.go:187-204`).
  This lease-expiry path is the relevant failure detector here: all three
  recoveries completed before the separate 30-second offline threshold. The
  5-second heartbeat and 1-second reconcile tick, followed by restore/start
  time, are consistent with the observed 20–25 seconds.
- Start grace is an extra lease for a newly created preparing/restoring
  execution (`apps/controlplane/internal/reconciler/reconciler.go:803-842`);
  it is not an additional 30 seconds before a running host's expired lease is
  reaped. The agent fencing margin was set to **1,000 ms** by the harness
  (`run_spike_c.py`, `--fence-margin-ms`); its code default is 5,000 ms
  (`apps/agent/src/config.rs:27-32,61-65`). The source was killed, so graceful
  fencing was not part of the measured host-loss path.
- The handler forces serial mode for testgame. Thus source stop/final-save,
  target replica readiness, and target restore are sequential costs. The
  configured factor 2 means one replica target beyond the producer; target
  selection and replica-factor behavior are in
  `apps/controlplane/internal/scheduler/scheduler.go:207-255`. The harness
  overrode the schema/group defaults (factor 3, min commit replicas 2,
  snapshot interval 120 seconds; `apps/controlplane/internal/store/types.go:47-53`,
  `apps/controlplane/internal/store/migrations_sqlite.sql:148-150`) with
  factor 2, min commit replicas 2, and interval 3600 seconds.

## Artifacts and cleanup

- Corrected planned-move and stable-forward samples:
  `/home/ubuntu/spikes/varde-c/results-1792770824.jsonl`
- Abrupt-loss samples and setup record:
  `/home/ubuntu/spikes/varde-c/results-1792770607.jsonl`
- CP and agent logs are in `/home/ubuntu/spikes/varde-c/logs-1792770824/`
  and `/home/ubuntu/spikes/varde-c/logs-1792770607/`.

The initial harness attempt hit a connection-refused race while the CP listener
was starting; the health check was made retry-safe before the measurement run.
One later startup log contained `epoch bump failed (concurrent activation)`;
the initial testgame execution became healthy and all measured moves completed.
The harness stopped the CP and agents on exit. A post-run process scan found no
Varde control-plane, agent, mesh, or testgame processes.

## Round 2: planned moves with reduced heartbeat timing

The C2 rerun used the same two-agent testgame harness and three planned moves,
with CP `--heartbeat-interval-ms 1000` and `--lease-ttl-ms 4000`. The 30-second
start grace, 1-second reconcile tick, replication factor 2, and minimum 2
commit replicas were unchanged. There is no separate agent polling knob: after
each successful heartbeat, the agent waits for the interval returned in the
CP response.

| Trial | Request → old host stopped | Request → replica ready | Request → new host healthy | Stable-forward downtime |
|---|---:|---:|---:|---:|
| 1 | 1.520 s | 2.533 s | 3.799 s | 3.750 s |
| 2 | 0.767 s | 1.525 s | 2.791 s | 2.750 s |
| 3 | 1.520 s | 2.532 s | 3.801 s | 3.750 s |

The phases shortened substantially from the original 9.6 / 14.67 / 19.74
second milestones. The old-host stop and replication-ready observations now
fall about one heartbeat apart; after replica readiness, target activation,
restore, and health contribute roughly another 1.27 seconds. This supports
heartbeat quantization as a major part of the original timings, while showing
that restore/start work remains. Stable-forward interruption followed the
overall move duration at the 250 ms probe resolution.

### Directive transport

Moves are not delivered over a push stream. `MoveServer` stores the serial
move's stop action and wakes reconciliation (`apps/controlplane/internal/api/handlers_servers.go:497-551`).
The CP's `AgentHeartbeat` handler builds and returns directives in the
`POST /v1/agent/heartbeat` response (`apps/controlplane/internal/api/handlers_agent.go:52-57,115-121`).
The agent sends that request and applies the returned `AgentDirectives`
(`apps/agent/src/control.rs:60-67`). Its apply loop gracefully stops an
existing execution whose directive has `action == "stop"` and starts a
newly-directed execution (`apps/agent/src/control.rs:237-288`). The control
loop uses the response's `heartbeat_interval_ms` for its next poll
(`apps/agent/src/control.rs:360-374`), so setting the CP heartbeat to 1,000 ms
also lowered the agent's normal poll interval; there is no distinct poll
interval flag in this path.

The C2 run completed all three migrations and the harness exited successfully.
The CP and agents were stopped by its cleanup; the post-run process scan found
no `varde-control-plane`, `varde-agent`, `varde-mesh`, or `varde-testgame`
processes. Results are in
`/home/ubuntu/spikes/varde-c/results-1792771531.jsonl`, with logs in the
corresponding `logs-1792771531/` directory.
