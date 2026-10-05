# Storage, snapshots & replication

Implemented in Rust: `crates/snapshot-store` (format, chunk store, snapshot/restore, GC — no networking) and the
agent's replication module (transfer over the mesh).

## Never replicate live files

A game's working directory (`servers/<srv_id>/`) is mutable and local. Only **immutable snapshots** taken behind a
game-specific *save barrier* (see `agent.md`) leave the machine.

## Chunk store

* Content-defined chunking: FastCDC (`fastcdc` crate, 2020 variant), min 256 KiB / avg 1 MiB / max 4 MiB.
  Files smaller than min are one chunk.
* Chunk id = BLAKE3 of the **plaintext**, lowercase hex.
* Stored compressed with zstd level 3 at `chunks/<first 2 hex>/<hash>`; written to `chunks/tmp/` then renamed (atomic).
* Every read decompresses and re-hashes; mismatch ⇒ error + chunk moved to `chunks/quarantine/` (I6).

## Manifest (format v1)

Canonical JSON (UTF-8, keys in fixed struct order, `files` sorted by `path` bytewise, no insignificant whitespace):

```jsonc
{
  "format": 1,
  "server_id": "srv_…", "execution_id": "exec_…", "epoch": 7, "node_id": "node_…",
  "parent": "snap_…" | null, "deployment_id": "dep_…",
  "created_at_unix_ms": 1790000000000, "reason": "scheduled",
  "files": [
    {"path": "world/level.dat", "type": "file", "size": 1234, "mode": 420, "mtime_unix_ms": 1790000000000,
     "blake3": "<whole-file hash>", "chunks": ["<hash>", "…"]},
    {"path": "world/region", "type": "dir", "mode": 493}
  ]
}
```

* `manifest_digest` = BLAKE3(manifest bytes); `snapshot_id` = `snap_` + base32(digest[0..16]) — so a manifest can
  never change under the same id (I5). Stored at `snapshots/<snapshot_id>.json`.
* Paths are relative, `/`-separated, validated on create **and** restore: no `..`, no absolute paths, no drive
  letters, no NUL, no Windows-reserved names (`CON`, `NUL`, …), no trailing dot/space. Symlinks and special files are
  skipped with a warning. `mode` is advisory (applied on Unix only).
* Which paths are included is decided by the driver (`persistent_paths()`), relative to the server working dir.

## Snapshot & restore API (crate)

```rust
pub struct Store { root: PathBuf }
impl Store {
    pub fn open(root: impl AsRef<Path>) -> Result<Store>;
    pub fn snapshot(&self, base: &Path, include: &[PathPattern], meta: SnapshotMeta) -> Result<SnapshotInfo>;
    pub fn restore(&self, snapshot_id: &SnapshotId, dest: &Path, include: &[PathPattern]) -> Result<()>; // staging dir + rename; removes included paths not in manifest
    pub fn manifest(&self, id: &SnapshotId) -> Result<Manifest>;
    pub fn manifest_bytes(&self, id: &SnapshotId) -> Result<Vec<u8>>;
    pub fn put_manifest_bytes(&self, bytes: &[u8]) -> Result<SnapshotId>;            // verifies digest
    pub fn missing_chunks(&self, m: &Manifest) -> Result<Vec<ChunkId>>;
    pub fn has_chunk(&self, id: &ChunkId) -> bool;
    pub fn read_chunk_compressed(&self, id: &ChunkId) -> Result<Vec<u8>>;           // for transfer
    pub fn put_chunk_compressed(&self, id: &ChunkId, zstd: &[u8]) -> Result<()>;   // verifies plaintext hash
    pub fn verify(&self, id: &SnapshotId) -> Result<VerifyReport>;
    pub fn delete_snapshot(&self, id: &SnapshotId) -> Result<()>;
    pub fn gc(&self) -> Result<GcReport>;  // mark = chunks referenced by remaining manifests; sweep the rest (skips chunks newer than 1h to avoid racing in-flight transfers)
}
```
`SnapshotInfo` returns id, digest, sizes, file/chunk counts and number of **new** chunks (dedup visibility).

## Snapshot lifecycle

`local` (accepted by CP, replica only on producer) → `replicating` (≥1 extra replica assigned) → `committed` (policy
satisfied) → `superseded` (newer committed exists; still restorable until retention deletes it). `invalid` = failed
verification or produced by a fenced execution (never uploaded in the first place).

**Commit policy** (evaluated by the CP whenever a replica becomes ready):
`ready_replicas ≥ min_commit_replicas` **and**, if the group sets `require_anchor_for_commit` and has at least one
anchor, `≥ 1` ready replica is on an anchor. For a single-node group `min_commit_replicas` is clamped to the number of
enrolled nodes so a solo player still gets "safe" saves (they're just on one machine, which the UI says).

## Replication

The CP decides targets; agents move bytes.

* **Target selection** (deterministic): all online anchors first (up to `replication_factor − 1`), then other online
  nodes by score: already has parent snapshot +50 (dedup) · hosting enabled +20 · free disk headroom · node_id
  tie-break. Nodes that come online later are assigned retroactively until `replication_factor` ready replicas exist
  for the newest committed snapshot (and for any newer not-yet-committed one).
* Targets receive `replication_tasks` in their heartbeat directives. A task is idempotent: the agent fetches the
  manifest, computes `missing_chunks`, downloads them from any `source_node_ids` (round-robin, resume = just ask for
  what's still missing), verifies, then reports `ready`. Parent-first is not required: dedup is chunk-level.
* **Transfer** goes through the mesh, not the CP: each agent serves its store on `127.0.0.1:<ephemeral>` as plain
  HTTP/1.1 and registers it with its mesh as internal service `agent.chunks`. To fetch from a peer, the agent calls
  `BindInternalForward(peer, "agent.chunks")` and issues requests to the returned local address:
  `GET /v1/manifests/{snapshot_id}` (raw bytes), `GET /v1/chunks/{hash}` (zstd bytes),
  `POST /v1/chunks/batch {hashes:[…]}` → concatenated `[u32 len][32-byte hash][zstd]` frames.
  The receiving side verifies every chunk before storing.
* Concurrency: 4 chunk requests in flight per source, bandwidth caps configurable per node.

## Anchors

An anchor is a node with `anchor = true` (set from the dashboard or by its enrollment token). Effects:

1. **Always a replication target** for every server in the group (it gets every snapshot, not just `replication_factor`
   worth), so it always has the latest save.
2. Optional group policy `require_anchor_for_commit`: a save counts as safe only once an anchor has it.
3. **Graceful host shutdown** (agent stopping, OS shutdown, `stop` action): the host performs a final barrier +
   snapshot and actively *pushes* priority to replication: it holds the stop until an anchor (or, without one, any
   peer) reports the snapshot ready, bounded by `shutdown_replication_timeout` (default 60 s on Linux; on Windows the
   service requests a pre-shutdown timeout of 120 s). If it times out the snapshot remains `local`, the UI shows
   "Latest save only on Arne-PC" and replication resumes when that PC is back.
4. Scheduler bonus +80 as host when hosting is enabled on it ("always-on").

Anchors run the normal agent (`p2pgames-agent run --anchor` is equivalent to setting the flag; typically
`hosting_enabled=false`). A hosting provider can offer "cloud backup" simply by enrolling anchor nodes into customer
groups.

## Retention & deletion

Per server keep: the newest `snapshot_retention` (default 10) committed snapshots, all pinned ones, any snapshot newer
than the newest committed, and the parent chain needed for nothing (chunks are shared, not deltas, so any snapshot is
independently restorable). The CP lists others in `delete_snapshots` for the nodes holding them; agents delete the
manifest and run `gc()` periodically. Agents **never** delete snapshots on their own initiative.
