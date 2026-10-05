CREATE TABLE IF NOT EXISTS kv (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS users (
    id            TEXT PRIMARY KEY,
    email         TEXT NOT NULL UNIQUE,
    display_name  TEXT NOT NULL,
    password_hash TEXT NOT NULL,
    is_operator   INTEGER NOT NULL DEFAULT 0,
    created_at    BIGINT NOT NULL
);

CREATE TABLE IF NOT EXISTS sessions (
    token_hash TEXT PRIMARY KEY,
    user_id    TEXT NOT NULL REFERENCES users(id),
    created_at BIGINT NOT NULL,
    expires_at BIGINT NOT NULL
);

CREATE TABLE IF NOT EXISTS groups (
    id            TEXT PRIMARY KEY,
    name          TEXT NOT NULL,
    plan          TEXT NOT NULL DEFAULT 'free',
    settings_json TEXT NOT NULL DEFAULT '{}',
    created_at    BIGINT NOT NULL
);

CREATE TABLE IF NOT EXISTS group_limits (
    group_id              TEXT PRIMARY KEY REFERENCES groups(id),
    max_nodes             BIGINT,
    max_servers           BIGINT,
    max_storage_bytes     BIGINT,
    max_relay_bytes_month BIGINT
);

CREATE TABLE IF NOT EXISTS group_members (
    group_id   TEXT NOT NULL REFERENCES groups(id),
    user_id    TEXT NOT NULL REFERENCES users(id),
    role       TEXT NOT NULL CHECK (role IN ('owner','admin','member')),
    created_at BIGINT NOT NULL,
    PRIMARY KEY (group_id, user_id)
);

CREATE TABLE IF NOT EXISTS invites (
    code       TEXT PRIMARY KEY,
    group_id   TEXT NOT NULL REFERENCES groups(id),
    role       TEXT NOT NULL DEFAULT 'member',
    created_by TEXT NOT NULL,
    expires_at BIGINT,
    max_uses   BIGINT,
    uses       BIGINT NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS enrollment_tokens (
    id              TEXT PRIMARY KEY,
    group_id        TEXT NOT NULL REFERENCES groups(id),
    token_hash      TEXT NOT NULL UNIQUE,
    anchor          INTEGER NOT NULL DEFAULT 0,
    hosting_enabled INTEGER NOT NULL DEFAULT 1,
    labels_json     TEXT NOT NULL DEFAULT '{}',
    expires_at      BIGINT,
    max_uses        BIGINT,
    uses            BIGINT NOT NULL DEFAULT 0,
    created_by      TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS device_links (
    user_code        TEXT PRIMARY KEY,
    device_code_hash TEXT NOT NULL UNIQUE,
    public_key       TEXT NOT NULL,
    hostname         TEXT NOT NULL DEFAULT '',
    os               TEXT NOT NULL DEFAULT '',
    arch             TEXT NOT NULL DEFAULT '',
    agent_version    TEXT NOT NULL DEFAULT '',
    state            TEXT NOT NULL DEFAULT 'pending'
                     CHECK (state IN ('pending','approved','consumed','expired')),
    group_id         TEXT,
    node_id          TEXT,
    approved_by      TEXT,
    expires_at       BIGINT NOT NULL,
    created_at       BIGINT NOT NULL
);

CREATE TABLE IF NOT EXISTS nodes (
    id              TEXT PRIMARY KEY,
    group_id        TEXT NOT NULL REFERENCES groups(id),
    name            TEXT NOT NULL,
    public_key      TEXT NOT NULL UNIQUE,
    os              TEXT NOT NULL DEFAULT '',
    arch            TEXT NOT NULL DEFAULT '',
    agent_version   TEXT NOT NULL DEFAULT '',
    hosting_enabled INTEGER NOT NULL DEFAULT 1,
    anchor          INTEGER NOT NULL DEFAULT 0,
    priority        BIGINT NOT NULL DEFAULT 0,
    max_memory_mb   BIGINT,
    max_cpu_percent BIGINT,
    max_storage_bytes BIGINT,
    admin_state     TEXT NOT NULL DEFAULT 'active'
                    CHECK (admin_state IN ('active','draining','disabled')),
    owner_user_id   TEXT REFERENCES users(id),
    last_seen_at    BIGINT,
    created_at      BIGINT NOT NULL
);

CREATE TABLE IF NOT EXISTS node_status (
    node_id     TEXT PRIMARY KEY REFERENCES nodes(id),
    status_json TEXT NOT NULL DEFAULT '{}',
    mesh_json   TEXT NOT NULL DEFAULT '{}',
    updated_at  BIGINT NOT NULL
);

CREATE TABLE IF NOT EXISTS deployments (
    id         TEXT PRIMARY KEY,
    group_id   TEXT NOT NULL REFERENCES groups(id),
    game_id    TEXT NOT NULL,
    spec_json  TEXT NOT NULL DEFAULT '{}',
    digest     TEXT NOT NULL DEFAULT '',
    created_at BIGINT NOT NULL
);

CREATE TABLE IF NOT EXISTS servers (
    id                   TEXT PRIMARY KEY,
    group_id             TEXT NOT NULL REFERENCES groups(id),
    name                 TEXT NOT NULL,
    game_id              TEXT NOT NULL,
    deployment_id        TEXT,
    desired_state        TEXT NOT NULL DEFAULT 'stopped'
                         CHECK (desired_state IN ('running','stopped')),
    observed_state       TEXT NOT NULL DEFAULT 'stopped',
    replication_factor   BIGINT NOT NULL DEFAULT 3,
    min_commit_replicas  BIGINT NOT NULL DEFAULT 2,
    snapshot_interval_s  BIGINT NOT NULL DEFAULT 120,
    preferred_node_id    TEXT,
    epoch                BIGINT NOT NULL DEFAULT 0,
    config_json          TEXT NOT NULL DEFAULT '{}',
    created_at           BIGINT NOT NULL,
    updated_at           BIGINT NOT NULL
);

CREATE TABLE IF NOT EXISTS services (
    id          TEXT PRIMARY KEY,
    server_id   TEXT NOT NULL UNIQUE REFERENCES servers(id),
    group_id    TEXT NOT NULL REFERENCES groups(id),
    loopback_ip TEXT NOT NULL,
    ports_json  TEXT NOT NULL DEFAULT '[]',
    UNIQUE (group_id, loopback_ip)
);

CREATE TABLE IF NOT EXISTS server_executions (
    id                  TEXT PRIMARY KEY,
    server_id           TEXT NOT NULL REFERENCES servers(id),
    node_id             TEXT NOT NULL REFERENCES nodes(id),
    epoch               BIGINT NOT NULL,
    state               TEXT NOT NULL DEFAULT 'preparing',
    action              TEXT NOT NULL DEFAULT 'run' CHECK (action IN ('run','stop')),
    stop_reason         TEXT,
    lease_expires_at    BIGINT NOT NULL,
    restore_snapshot_id TEXT,
    placement_json      TEXT NOT NULL DEFAULT '{}',
    health              TEXT,
    message             TEXT,
    created_at          BIGINT NOT NULL,
    started_at          BIGINT,
    ended_at            BIGINT,
    end_reason          TEXT
);
CREATE UNIQUE INDEX IF NOT EXISTS one_active
    ON server_executions(server_id) WHERE ended_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_exec_node ON server_executions(node_id) WHERE ended_at IS NULL;

CREATE TABLE IF NOT EXISTS migrations (
    id                 TEXT PRIMARY KEY,
    server_id          TEXT NOT NULL REFERENCES servers(id),
    from_execution_id  TEXT NOT NULL,
    to_node_id         TEXT NOT NULL REFERENCES nodes(id),
    state              TEXT NOT NULL DEFAULT 'stopping'
                       CHECK (state IN ('stopping','await_replication','activating','done','failed')),
    final_snapshot_id  TEXT,
    created_at         BIGINT NOT NULL,
    finished_at        BIGINT
);

CREATE TABLE IF NOT EXISTS snapshots (
    id              TEXT PRIMARY KEY,
    server_id       TEXT NOT NULL REFERENCES servers(id),
    group_id        TEXT NOT NULL REFERENCES groups(id),
    epoch           BIGINT NOT NULL,
    execution_id    TEXT NOT NULL,
    node_id         TEXT NOT NULL,
    parent_id       TEXT,
    manifest_digest TEXT NOT NULL,
    deployment_id   TEXT NOT NULL DEFAULT '',
    reason          TEXT NOT NULL CHECK (reason IN ('scheduled','manual','final','migration')),
    state           TEXT NOT NULL DEFAULT 'local'
                    CHECK (state IN ('local','replicating','committed','superseded','invalid')),
    size_bytes      BIGINT NOT NULL DEFAULT 0,
    stored_bytes    BIGINT NOT NULL DEFAULT 0,
    file_count      BIGINT NOT NULL DEFAULT 0,
    chunk_count     BIGINT NOT NULL DEFAULT 0,
    pinned          INTEGER NOT NULL DEFAULT 0,
    created_at      BIGINT NOT NULL,
    committed_at    BIGINT
);
CREATE INDEX IF NOT EXISTS idx_snap_server ON snapshots(server_id, created_at);

CREATE TABLE IF NOT EXISTS snapshot_replicas (
    snapshot_id TEXT NOT NULL REFERENCES snapshots(id),
    node_id     TEXT NOT NULL REFERENCES nodes(id),
    state       TEXT NOT NULL DEFAULT 'assigned'
                CHECK (state IN ('assigned','ready','deleting')),
    assigned_at BIGINT NOT NULL,
    ready_at    BIGINT,
    PRIMARY KEY (snapshot_id, node_id)
);

-- Pending out-of-band snapshot requests delivered via the next heartbeat.
CREATE TABLE IF NOT EXISTS snapshot_requests (
    id         TEXT PRIMARY KEY,
    server_id  TEXT NOT NULL REFERENCES servers(id),
    reason     TEXT NOT NULL DEFAULT 'manual',
    created_at BIGINT NOT NULL,
    claimed    INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS events (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    group_id   TEXT NOT NULL,
    server_id  TEXT,
    node_id    TEXT,
    type       TEXT NOT NULL,
    data_json  TEXT NOT NULL DEFAULT '{}',
    created_at BIGINT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_events_group ON events(group_id, id);

CREATE TABLE IF NOT EXISTS audit_log (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    group_id   TEXT,
    actor_type TEXT NOT NULL CHECK (actor_type IN ('user','node','system')),
    actor_id   TEXT NOT NULL,
    action     TEXT NOT NULL,
    target     TEXT NOT NULL DEFAULT '',
    data_json  TEXT NOT NULL DEFAULT '{}',
    created_at BIGINT NOT NULL
);

CREATE TABLE IF NOT EXISTS execution_logs (
    execution_id TEXT NOT NULL REFERENCES server_executions(id),
    seq          BIGINT NOT NULL,
    at           BIGINT NOT NULL,
    stream       TEXT NOT NULL CHECK (stream IN ('stdout','stderr','agent')),
    line         TEXT NOT NULL,
    PRIMARY KEY (execution_id, seq)
);
