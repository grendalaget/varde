//! Request/response types for the control-plane API. Handwritten to match
//! api/openapi/control-plane.yaml; `tests/openapi_drift.rs` fails if the
//! agent schemas drift out of sync.

use serde::{Deserialize, Deserializer, Serialize};

/// Go's JSON encoder emits `null` (not `[]`) for empty slices — treat both as
/// an empty Vec.
fn null_vec<'de, D, T>(d: D) -> Result<Vec<T>, D::Error>
where
    D: Deserializer<'de>,
    T: Deserialize<'de>,
{
    Ok(Option::<Vec<T>>::deserialize(d)?.unwrap_or_default())
}

// ---------- enrollment ----------

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct DeviceEnrollRequest {
    pub public_key: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub hostname: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub os: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub arch: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub agent_version: Option<String>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct DeviceEnrollResponse {
    pub device_code: String,
    pub user_code: String,
    pub verification_url: String,
    pub expires_in: i64,
    pub interval: i64,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct DevicePollRequest {
    pub device_code: String,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct TokenEnrollRequest {
    pub token: String,
    pub public_key: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub hostname: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub os: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub arch: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub agent_version: Option<String>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct EnrollResult {
    pub node_id: String,
    pub group_id: String,
    pub control_plane_public_key: String,
}

// ---------- heartbeat / directives ----------

#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct Capabilities {
    #[serde(skip_serializing_if = "Option::is_none")]
    pub os: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub arch: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub cpu_cores: Option<i64>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub memory_total_mb: Option<i64>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub memory_available_mb: Option<i64>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub disk_free_bytes: Option<i64>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub on_battery: Option<bool>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub user_active: Option<bool>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub runtimes: Option<Vec<String>>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub cached_deployments: Option<Vec<String>>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub drivers: Option<Vec<String>>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub uptime_s: Option<i64>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub agent_started_at: Option<i64>,
    /// true while the agent drains executions on shutdown
    #[serde(skip_serializing_if = "Option::is_none")]
    pub draining: Option<bool>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct MeshPeerReport {
    #[serde(skip_serializing_if = "Option::is_none")]
    pub node_id: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub path: Option<String>, // none|direct|relayed
    #[serde(skip_serializing_if = "Option::is_none")]
    pub rtt_us: Option<i64>,
}

#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct MeshReport {
    #[serde(skip_serializing_if = "Option::is_none")]
    pub listen_port: Option<i64>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub local_endpoints: Option<Vec<String>>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub observed_endpoints: Option<Vec<String>>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub relay_ids: Option<Vec<String>>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub peers: Option<Vec<MeshPeerReport>>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ExecutionReport {
    pub execution_id: String,
    pub server_id: String,
    pub epoch: i64,
    pub state: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub health: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub message: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub player_count: Option<i64>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct AgentHeartbeat {
    #[serde(skip_serializing_if = "Option::is_none")]
    pub agent_version: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub capabilities: Option<Capabilities>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub mesh: Option<MeshReport>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub executions: Option<Vec<ExecutionReport>>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub snapshots_stored_bytes: Option<i64>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct DirectivePeer {
    pub node_id: String,
    pub name: String,
    pub public_key: String,
    #[serde(default, deserialize_with = "null_vec")]
    pub endpoints: Vec<String>,
    #[serde(default, deserialize_with = "null_vec")]
    pub relay_ids: Vec<String>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct GamePort {
    pub port: i64,
    pub protocol: String,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Service {
    pub service_id: String,
    pub loopback_ip: String,
    #[serde(default, deserialize_with = "null_vec")]
    pub ports: Vec<GamePort>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct DirectiveRoute {
    pub service_id: String,
    pub server_id: String,
    pub server_name: String,
    pub loopback_ip: String,
    #[serde(default, deserialize_with = "null_vec")]
    pub ports: Vec<GamePort>,
    #[serde(default)]
    pub host_node_id: String,
    #[serde(default)]
    pub epoch: i64,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct DirectiveRelay {
    pub relay_id: String,
    pub addr: String,
    pub token: String,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ReplicationTask {
    pub snapshot_id: String,
    pub server_id: String,
    pub manifest_digest: String,
    #[serde(default, deserialize_with = "null_vec")]
    pub source_node_ids: Vec<String>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct SnapshotRequest {
    pub request_id: String,
    pub reason: String,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Deployment {
    pub id: String,
    pub game_id: String,
    pub spec: serde_json::Value,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct RestoreDirective {
    pub snapshot_id: String,
    pub manifest_digest: String,
    #[serde(default, deserialize_with = "null_vec")]
    pub source_node_ids: Vec<String>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ExecutionDirective {
    pub execution_id: String,
    pub server_id: String,
    pub server_name: String,
    pub epoch: i64,
    pub lease_expires_at_unix_ms: i64,
    pub action: String, // run|stop
    #[serde(default)]
    pub stop_reason: Option<String>,
    #[serde(default)]
    pub deployment: Option<Deployment>,
    #[serde(default)]
    pub config: Option<serde_json::Value>,
    #[serde(default)]
    pub service: Option<Service>,
    #[serde(default)]
    pub restore: Option<RestoreDirective>,
    #[serde(default)]
    pub snapshot_interval_s: Option<i64>,
    #[serde(default, deserialize_with = "null_vec")]
    pub snapshot_requests: Vec<SnapshotRequest>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct NodeFlags {
    pub name: String,
    pub hosting_enabled: bool,
    pub anchor: bool,
    pub admin_state: String,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct AgentDirectives {
    pub server_time_unix_ms: i64,
    pub heartbeat_interval_ms: i64,
    pub lease_ttl_ms: i64,
    pub node: NodeFlags,
    #[serde(default, deserialize_with = "null_vec")]
    pub executions: Vec<ExecutionDirective>,
    #[serde(default, deserialize_with = "null_vec")]
    pub peers: Vec<DirectivePeer>,
    #[serde(default, deserialize_with = "null_vec")]
    pub routes: Vec<DirectiveRoute>,
    #[serde(default, deserialize_with = "null_vec")]
    pub relays: Vec<DirectiveRelay>,
    #[serde(default, deserialize_with = "null_vec")]
    pub replication_tasks: Vec<ReplicationTask>,
    #[serde(default, deserialize_with = "null_vec")]
    pub delete_snapshots: Vec<String>,
}

// ---------- other agent writes ----------

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ExecutionStatusUpdate {
    pub server_id: String,
    pub epoch: i64,
    pub state: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub health: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub message: Option<String>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct LogLine {
    pub at: i64,
    pub stream: String,
    pub line: String,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct LogBatch {
    pub lines: Vec<LogLine>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct AgentSnapshot {
    pub snapshot_id: String,
    pub server_id: String,
    pub execution_id: String,
    pub epoch: i64,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub parent_id: Option<String>,
    pub manifest_digest: String,
    pub deployment_id: String,
    pub reason: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub size_bytes: Option<i64>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub stored_bytes: Option<i64>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub file_count: Option<i64>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub chunk_count: Option<i64>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub request_id: Option<String>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ReplicaReady {
    pub state: String, // "ready"
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ErrorBody {
    pub code: String,
    pub message: String,
    #[serde(default)]
    pub details: Option<serde_json::Value>,
}

// ---------- public API (used by tooling/tests) ----------

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct SignupRequest {
    pub email: String,
    pub password: String,
    pub display_name: String,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct LoginRequest {
    pub email: String,
    pub password: String,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct AuthUser {
    pub id: String,
    pub email: String,
    #[serde(default)]
    pub display_name: String,
    #[serde(default)]
    pub is_operator: bool,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct AuthResponse {
    pub user: AuthUser,
    pub token: String,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct CreateGroupRequest {
    pub name: String,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Group {
    pub id: String,
    pub name: String,
    #[serde(default)]
    pub plan: String,
    #[serde(default)]
    pub settings: Option<serde_json::Value>,
    #[serde(default)]
    pub role: Option<String>,
    #[serde(default)]
    pub created_at: Option<i64>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct CreateEnrollmentTokenRequest {
    #[serde(skip_serializing_if = "Option::is_none")]
    pub anchor: Option<bool>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub hosting_enabled: Option<bool>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub expires_at: Option<i64>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub max_uses: Option<i64>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub labels: Option<serde_json::Value>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct EnrollmentTokenCreated {
    pub id: String,
    #[serde(default)]
    pub token: String,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct CreateServerRequest {
    pub name: String,
    pub game_id: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub config: Option<serde_json::Value>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub replication_factor: Option<i64>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub min_commit_replicas: Option<i64>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub snapshot_interval_s: Option<i64>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub preferred_node_id: Option<String>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct StartServerRequest {
    #[serde(skip_serializing_if = "Option::is_none")]
    pub preferred_node_id: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub allow_older_snapshot: Option<bool>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct MoveServerRequest {
    pub target_node_id: String,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ReplicaView {
    pub node_id: String,
    #[serde(default)]
    pub name: Option<String>,
    #[serde(default)]
    pub anchor: Option<bool>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct SaveRef {
    pub snapshot_id: String,
    pub created_at: i64,
    #[serde(default)]
    pub replicas: Vec<ReplicaView>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct HostingOn {
    pub node_id: String,
    pub name: String,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ServerSummary {
    #[serde(default)]
    pub hosting_on: Option<HostingOn>,
    #[serde(default)]
    pub latest_safe_save: Option<SaveRef>,
    #[serde(default)]
    pub latest_save: Option<SaveRef>,
    #[serde(default)]
    pub only_on_one_machine: Option<bool>,
    #[serde(default)]
    pub address: Option<String>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Server {
    pub id: String,
    pub group_id: String,
    pub name: String,
    pub game_id: String,
    #[serde(default)]
    pub deployment_id: Option<String>,
    #[serde(default)]
    pub desired_state: String,
    #[serde(default)]
    pub observed_state: String,
    #[serde(default)]
    pub epoch: i64,
    #[serde(default)]
    pub service: Option<Service>,
    #[serde(default)]
    pub summary: Option<ServerSummary>,
    #[serde(default)]
    pub config: Option<serde_json::Value>,
    #[serde(default)]
    pub snapshot_interval_s: Option<i64>,
    #[serde(default)]
    pub created_at: i64,
    #[serde(default)]
    pub updated_at: i64,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Snapshot {
    pub id: String,
    pub server_id: String,
    pub epoch: i64,
    #[serde(default)]
    pub execution_id: String,
    #[serde(default)]
    pub node_id: String,
    #[serde(default)]
    pub parent_id: Option<String>,
    #[serde(default)]
    pub manifest_digest: String,
    #[serde(default)]
    pub reason: String,
    #[serde(default)]
    pub state: String,
    #[serde(default)]
    pub pinned: bool,
    #[serde(default)]
    pub created_at: i64,
    #[serde(default)]
    pub replicas: Vec<serde_json::Value>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Execution {
    pub id: String,
    pub server_id: String,
    pub node_id: String,
    pub epoch: i64,
    #[serde(default)]
    pub state: String,
    #[serde(default)]
    pub action: String,
    #[serde(default)]
    pub stop_reason: Option<String>,
    #[serde(default)]
    pub lease_expires_at: Option<i64>,
    #[serde(default)]
    pub restore_snapshot_id: Option<String>,
    #[serde(default)]
    pub health: Option<String>,
    #[serde(default)]
    pub message: Option<String>,
    #[serde(default)]
    pub started_at: Option<i64>,
    #[serde(default)]
    pub ended_at: Option<i64>,
    #[serde(default)]
    pub end_reason: Option<String>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct RelayPublicKey {
    pub public_key: String,
}
