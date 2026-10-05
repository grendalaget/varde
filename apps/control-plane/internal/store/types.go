package store

import (
	"encoding/json"

	"github.com/jmoiron/sqlx/types"
)

// Row types mirror the tables in docs/architecture/control-plane.md.
// Timestamps are unix milliseconds; JSON columns keep raw text here.

type User struct {
	ID           string `db:"id"`
	Email        string `db:"email"`
	DisplayName  string `db:"display_name"`
	PasswordHash string `db:"password_hash"`
	IsOperator   int64  `db:"is_operator"`
	CreatedAt    int64  `db:"created_at"`
}

type Session struct {
	TokenHash string `db:"token_hash"`
	UserID    string `db:"user_id"`
	CreatedAt int64  `db:"created_at"`
	ExpiresAt int64  `db:"expires_at"`
}

type Group struct {
	ID           string `db:"id"`
	Name         string `db:"name"`
	Plan         string `db:"plan"`
	SettingsJSON string `db:"settings_json"`
	CreatedAt    int64  `db:"created_at"`
}

// GroupSettings is the JSON shape stored in groups.settings_json.
type GroupSettings struct {
	SnapshotIntervalS        int64 `json:"snapshot_interval_s"`
	DefaultReplicationFactor int64 `json:"default_replication_factor"`
	DefaultMinCommitReplicas int64 `json:"default_min_commit_replicas"`
	RequireAnchorForCommit   bool  `json:"require_anchor_for_commit"`
	SnapshotRetention        int64 `json:"snapshot_retention"`
}

func DefaultGroupSettings() GroupSettings {
	return GroupSettings{
		SnapshotIntervalS:        120,
		DefaultReplicationFactor: 3,
		DefaultMinCommitReplicas: 2,
		SnapshotRetention:        10,
	}
}

func (g *Group) Settings() GroupSettings {
	s := DefaultGroupSettings()
	_ = json.Unmarshal([]byte(g.SettingsJSON), &s)
	return s
}

type GroupLimits struct {
	GroupID          string `db:"group_id"`
	MaxNodes         *int64 `db:"max_nodes"`
	MaxServers       *int64 `db:"max_servers"`
	MaxStorageBytes  *int64 `db:"max_storage_bytes"`
	MaxRelayBytesMon *int64 `db:"max_relay_bytes_month"`
}

type Member struct {
	GroupID   string `db:"group_id"`
	UserID    string `db:"user_id"`
	Role      string `db:"role"`
	CreatedAt int64  `db:"created_at"`
	// joined from users when listing
	Email       string `db:"email"`
	DisplayName string `db:"display_name"`
}

type Invite struct {
	Code      string `db:"code"`
	GroupID   string `db:"group_id"`
	Role      string `db:"role"`
	CreatedBy string `db:"created_by"`
	ExpiresAt *int64 `db:"expires_at"`
	MaxUses   *int64 `db:"max_uses"`
	Uses      int64  `db:"uses"`
}

type EnrollmentToken struct {
	ID             string `db:"id"`
	GroupID        string `db:"group_id"`
	TokenHash      string `db:"token_hash"`
	Anchor         int64  `db:"anchor"`
	HostingEnabled int64  `db:"hosting_enabled"`
	LabelsJSON     string `db:"labels_json"`
	ExpiresAt      *int64 `db:"expires_at"`
	MaxUses        *int64 `db:"max_uses"`
	Uses           int64  `db:"uses"`
	CreatedBy      string `db:"created_by"`
}

type DeviceLink struct {
	UserCode       string  `db:"user_code"`
	DeviceCodeHash string  `db:"device_code_hash"`
	PublicKey      string  `db:"public_key"`
	Hostname       string  `db:"hostname"`
	OS             string  `db:"os"`
	Arch           string  `db:"arch"`
	AgentVersion   string  `db:"agent_version"`
	State          string  `db:"state"`
	GroupID        *string `db:"group_id"`
	NodeID         *string `db:"node_id"`
	ApprovedBy     *string `db:"approved_by"`
	ExpiresAt      int64   `db:"expires_at"`
	CreatedAt      int64   `db:"created_at"`
}

type Node struct {
	ID              string `db:"id"`
	GroupID         string `db:"group_id"`
	Name            string `db:"name"`
	PublicKey       string `db:"public_key"`
	OS              string `db:"os"`
	Arch            string `db:"arch"`
	AgentVersion    string `db:"agent_version"`
	HostingEnabled  int64  `db:"hosting_enabled"`
	Anchor          int64  `db:"anchor"`
	Priority        int64  `db:"priority"`
	MaxMemoryMB     *int64 `db:"max_memory_mb"`
	MaxCPUPercent   *int64 `db:"max_cpu_percent"`
	MaxStorageBytes *int64 `db:"max_storage_bytes"`
	AdminState      string `db:"admin_state"`
	LastSeenAt      *int64 `db:"last_seen_at"`
	CreatedAt       int64  `db:"created_at"`
}

type NodeStatus struct {
	NodeID     string `db:"node_id"`
	StatusJSON string `db:"status_json"`
	MeshJSON   string `db:"mesh_json"`
	UpdatedAt  int64  `db:"updated_at"`
}

type Deployment struct {
	ID        string `db:"id"`
	GroupID   string `db:"group_id"`
	GameID    string `db:"game_id"`
	SpecJSON  string `db:"spec_json"`
	Digest    string `db:"digest"`
	CreatedAt int64  `db:"created_at"`
}

type Server struct {
	ID                string  `db:"id"`
	GroupID           string  `db:"group_id"`
	Name              string  `db:"name"`
	GameID            string  `db:"game_id"`
	DeploymentID      *string `db:"deployment_id"`
	DesiredState      string  `db:"desired_state"`
	ObservedState     string  `db:"observed_state"`
	ReplicationFactor int64   `db:"replication_factor"`
	MinCommitReplicas int64   `db:"min_commit_replicas"`
	SnapshotIntervalS int64   `db:"snapshot_interval_s"`
	PreferredNodeID   *string `db:"preferred_node_id"`
	Epoch             int64   `db:"epoch"`
	ConfigJSON        string  `db:"config_json"`
	CreatedAt         int64   `db:"created_at"`
	UpdatedAt         int64   `db:"updated_at"`
}

type Service struct {
	ID         string `db:"id"`
	ServerID   string `db:"server_id"`
	GroupID    string `db:"group_id"`
	LoopbackIP string `db:"loopback_ip"`
	PortsJSON  string `db:"ports_json"`
}

type Execution struct {
	ID                string  `db:"id"`
	ServerID          string  `db:"server_id"`
	NodeID            string  `db:"node_id"`
	Epoch             int64   `db:"epoch"`
	State             string  `db:"state"`
	Action            string  `db:"action"`
	StopReason        *string `db:"stop_reason"`
	LeaseExpiresAt    int64   `db:"lease_expires_at"`
	RestoreSnapshotID *string `db:"restore_snapshot_id"`
	PlacementJSON     string  `db:"placement_json"`
	Health            *string `db:"health"`
	Message           *string `db:"message"`
	CreatedAt         int64   `db:"created_at"`
	StartedAt         *int64  `db:"started_at"`
	EndedAt           *int64  `db:"ended_at"`
	EndReason         *string `db:"end_reason"`
}

type Migration struct {
	ID              string  `db:"id"`
	ServerID        string  `db:"server_id"`
	FromExecutionID string  `db:"from_execution_id"`
	ToNodeID        string  `db:"to_node_id"`
	State           string  `db:"state"`
	FinalSnapshotID *string `db:"final_snapshot_id"`
	CreatedAt       int64   `db:"created_at"`
	FinishedAt      *int64  `db:"finished_at"`
}

type Snapshot struct {
	ID             string  `db:"id"`
	ServerID       string  `db:"server_id"`
	GroupID        string  `db:"group_id"`
	Epoch          int64   `db:"epoch"`
	ExecutionID    string  `db:"execution_id"`
	NodeID         string  `db:"node_id"`
	ParentID       *string `db:"parent_id"`
	ManifestDigest string  `db:"manifest_digest"`
	DeploymentID   string  `db:"deployment_id"`
	Reason         string  `db:"reason"`
	State          string  `db:"state"`
	SizeBytes      int64   `db:"size_bytes"`
	StoredBytes    int64   `db:"stored_bytes"`
	FileCount      int64   `db:"file_count"`
	ChunkCount     int64   `db:"chunk_count"`
	Pinned         int64   `db:"pinned"`
	CreatedAt      int64   `db:"created_at"`
	CommittedAt    *int64  `db:"committed_at"`
}

type Replica struct {
	SnapshotID string `db:"snapshot_id"`
	NodeID     string `db:"node_id"`
	State      string `db:"state"`
	AssignedAt int64  `db:"assigned_at"`
	ReadyAt    *int64 `db:"ready_at"`
}

type SnapshotRequest struct {
	ID        string `db:"id"`
	ServerID  string `db:"server_id"`
	Reason    string `db:"reason"`
	CreatedAt int64  `db:"created_at"`
	Claimed   int64  `db:"claimed"`
}

type Event struct {
	ID        int64   `db:"id"`
	GroupID   string  `db:"group_id"`
	ServerID  *string `db:"server_id"`
	NodeID    *string `db:"node_id"`
	Type      string  `db:"type"`
	DataJSON  string  `db:"data_json"`
	CreatedAt int64   `db:"created_at"`
}

type AuditEntry struct {
	ID        int64   `db:"id"`
	GroupID   *string `db:"group_id"`
	ActorType string  `db:"actor_type"`
	ActorID   string  `db:"actor_id"`
	Action    string  `db:"action"`
	Target    string  `db:"target"`
	DataJSON  string  `db:"data_json"`
	CreatedAt int64   `db:"created_at"`
}

type LogLineRow struct {
	ExecutionID string `db:"execution_id"`
	Seq         int64  `db:"seq"`
	At          int64  `db:"at"`
	Stream      string `db:"stream"`
	Line        string `db:"line"`
}

// JSONText is a convenience for optional JSON payload columns.
type JSONText = types.JSONText
