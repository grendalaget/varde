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
	// varde.games user id when the account came in through federation.
	WebsiteUserID    *string `db:"website_user_id"`
	SteamID          *string `db:"steam_id"`
	SteamPersonaName *string `db:"steam_persona_name"`
	SteamAvatarURL   *string `db:"steam_avatar_url"`
	CreatedAt        int64   `db:"created_at"`
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

type GroupPlugin struct {
	GroupID      string `db:"group_id"`
	PluginID     string `db:"plugin_id"`
	SettingsJSON string `db:"settings_json"`
	EnabledBy    string `db:"enabled_by"`
	CreatedAt    int64  `db:"created_at"`
	UpdatedAt    int64  `db:"updated_at"`
}

type ServerAddon struct {
	ServerID  string `db:"server_id"`
	PluginID  string `db:"plugin_id"`
	AddonID   string `db:"addon_id"`
	Spec      string `db:"spec"`
	CreatedAt int64  `db:"created_at"`
	UpdatedAt int64  `db:"updated_at"`
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
	Email       string  `db:"email"`
	DisplayName string  `db:"display_name"`
	SteamID     *string `db:"steam_id"`
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

// ServiceToken is a machine-to-machine credential (`vst_…` bearer) scoped to
// a narrow set of API operations — e.g. a provisioning service minting
// enrollment tokens without a user session.
type ServiceToken struct {
	ID           string  `db:"id"`
	Name         string  `db:"name"`
	TokenHash    string  `db:"token_hash"`
	ScopesJSON   string  `db:"scopes"`
	GroupIDsJSON *string `db:"group_ids"`
	CreatedAt    int64   `db:"created_at"`
	LastUsedAt   *int64  `db:"last_used_at"`
	RevokedAt    *int64  `db:"revoked_at"`
}

// Allows reports whether the token carries the given scope ("*" is a
// wildcard; an unknown or malformed scope list allows nothing).
func (t *ServiceToken) Allows(scope string) bool {
	var scopes []string
	if err := json.Unmarshal([]byte(t.ScopesJSON), &scopes); err != nil {
		return false
	}
	for _, s := range scopes {
		if s == scope || s == "*" {
			return true
		}
	}
	return false
}

// AllowsGroup reports whether the token may act in the given group. A NULL
// or empty group_ids list means unbound (infrastructure-wide); a malformed
// list denies. Unbound is the default because the first consumer, the
// anchor provisioner, must span every group — bind via --groups when a
// narrower credential is wanted.
func (t *ServiceToken) AllowsGroup(groupID string) bool {
	if t.GroupIDsJSON == nil {
		return true
	}
	var ids []string
	if err := json.Unmarshal([]byte(*t.GroupIDsJSON), &ids); err != nil {
		return false
	}
	if len(ids) == 0 {
		return true
	}
	for _, g := range ids {
		if g == groupID {
			return true
		}
	}
	return false
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
	ID              string  `db:"id"`
	GroupID         string  `db:"group_id"`
	Name            string  `db:"name"`
	PublicKey       string  `db:"public_key"`
	OS              string  `db:"os"`
	Arch            string  `db:"arch"`
	AgentVersion    string  `db:"agent_version"`
	UpdateAvailable string  `db:"update_available"`
	LeaseProfile    string  `db:"lease_profile"`
	HostingEnabled  int64   `db:"hosting_enabled"`
	Anchor          int64   `db:"anchor"`
	Priority        int64   `db:"priority"`
	MaxMemoryMB     *int64  `db:"max_memory_mb"`
	MaxCPUPercent   *int64  `db:"max_cpu_percent"`
	MaxStorageBytes *int64  `db:"max_storage_bytes"`
	AdminState      string  `db:"admin_state"`
	OwnerUserID     *string `db:"owner_user_id"`
	LastSeenAt      *int64  `db:"last_seen_at"`
	CreatedAt       int64   `db:"created_at"`
	// DeletedAt marks a removed machine; the row stays for the executions
	// and snapshots that reference it, but no lookup returns it.
	DeletedAt *int64 `db:"deleted_at"`
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
	// StandbyNodeID is the hot standby: the node a failover or shutdown
	// handoff would pick, kept pre-installed and replicated-to.
	StandbyNodeID *string `db:"standby_node_id"`
	Epoch         int64   `db:"epoch"`
	ConfigJSON    string  `db:"config_json"`
	CreatedAt     int64   `db:"created_at"`
	UpdatedAt     int64   `db:"updated_at"`
	// DeletedAt marks a deleted server waiting for machines to drop their
	// copies of its saves; PurgeDeletedServer removes the row afterwards.
	DeletedAt *int64 `db:"deleted_at"`
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
	// Role separates the one authoritative execution ('serving') from
	// migration-only executions: 'incoming' pre-stages on the target while
	// 'draining' is a former serving exec finishing its handoff.
	Role string `db:"role"`
}

type Migration struct {
	ID              string `db:"id"`
	ServerID        string `db:"server_id"`
	FromExecutionID string `db:"from_execution_id"`
	ToNodeID        string `db:"to_node_id"`
	State           string `db:"state"`
	// Mode: 'serial' = classic stop-then-start move; 'warm' = start-first
	// handoff for drivers that can transfer clients (e.g. Minecraft).
	Mode          string  `db:"mode"`
	ToExecutionID *string `db:"to_execution_id"`
	// DrainUntil bounds the mesh grace window that keeps old-epoch streams
	// alive after the route flip (warm mode only).
	DrainUntil      *int64  `db:"drain_until"`
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

type DirectiveFingerprintData struct {
	Groups     []string
	Executions []DirectiveExecutionFingerprint
	Migrations []DirectiveMigrationFingerprint
	Nodes      []DirectiveNodeFingerprint
	Replicas   []DirectiveReplicaFingerprint
	Events     []DirectiveEventFingerprint
	Servers    []DirectiveServerFingerprint
}

type DirectiveExecutionFingerprint struct {
	GroupID    string  `db:"group_id" json:"group_id"`
	ID         string  `db:"id" json:"id"`
	NodeID     string  `db:"node_id" json:"node_id"`
	State      string  `db:"state" json:"state"`
	Action     string  `db:"action" json:"action"`
	Epoch      int64   `db:"epoch" json:"epoch"`
	Role       string  `db:"role" json:"role"`
	StopReason *string `db:"stop_reason" json:"stop_reason"`
}

type DirectiveMigrationFingerprint struct {
	GroupID         string  `db:"group_id" json:"group_id"`
	ID              string  `db:"id" json:"id"`
	ServerID        string  `db:"server_id" json:"server_id"`
	FromExecutionID string  `db:"from_execution_id" json:"from_execution_id"`
	ToNodeID        string  `db:"to_node_id" json:"to_node_id"`
	State           string  `db:"state" json:"state"`
	Mode            string  `db:"mode" json:"mode"`
	ToExecutionID   *string `db:"to_execution_id" json:"to_execution_id"`
	DrainUntil      *int64  `db:"drain_until" json:"drain_until"`
	FinalSnapshotID *string `db:"final_snapshot_id" json:"final_snapshot_id"`
	CreatedAt       int64   `db:"created_at" json:"created_at"`
	FinishedAt      *int64  `db:"finished_at" json:"finished_at"`
}

type DirectiveNodeFingerprint struct {
	ID             string `db:"id"`
	GroupID        string `db:"group_id"`
	LeaseProfile   string `db:"lease_profile"`
	AdminState     string `db:"admin_state"`
	HostingEnabled int64  `db:"hosting_enabled"`
	Anchor         int64  `db:"anchor"`
	LastSeenAt     *int64 `db:"last_seen_at"`
}

type DirectiveReplicaFingerprint struct {
	GroupID    string `db:"group_id" json:"group_id"`
	SnapshotID string `db:"snapshot_id" json:"snapshot_id"`
	NodeID     string `db:"node_id" json:"node_id"`
	State      string `db:"state" json:"state"`
}

type DirectiveEventFingerprint struct {
	GroupID string `db:"group_id"`
	MaxID   int64  `db:"max_id"`
}

type DirectiveServerFingerprint struct {
	GroupID       string `db:"group_id" json:"group_id"`
	ID            string `db:"id" json:"id"`
	DesiredState  string `db:"desired_state" json:"desired_state"`
	ObservedState string `db:"observed_state" json:"observed_state"`
	Epoch         int64  `db:"epoch" json:"epoch"`
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

// CrashReport is one crash report uploaded by a linked machine.
type CrashReport struct {
	ID          string  `db:"id"`
	GroupID     string  `db:"group_id"`
	NodeID      string  `db:"node_id"`
	Kind        string  `db:"kind"`
	Exe         string  `db:"exe"`
	Version     string  `db:"version"`
	Os          string  `db:"os"`
	Arch        string  `db:"arch"`
	Pid         int64   `db:"pid"`
	Message     string  `db:"message"`
	Location    string  `db:"location"`
	Backtrace   string  `db:"backtrace"`
	Detail      string  `db:"detail"`
	CreatedAt   int64   `db:"created_at"`
	ReceivedAt  int64   `db:"received_at"`
	Dump        *[]byte `db:"dump"`
	DumpName    string  `db:"dump_name"`
	DumpOmitted string  `db:"dump_omitted"`
	ClientRef   string  `db:"client_ref"`
}

// CrashReportMeta is CrashReport without the dump body (list views).
type CrashReportMeta struct {
	ID          string `db:"id"`
	NodeID      string `db:"node_id"`
	NodeName    string `db:"node_name"`
	Kind        string `db:"kind"`
	Exe         string `db:"exe"`
	Version     string `db:"version"`
	Message     string `db:"message"`
	CreatedAt   int64  `db:"created_at"`
	ReceivedAt  int64  `db:"received_at"`
	DumpSize    int64  `db:"dump_size"`
	DumpOmitted string `db:"dump_omitted"`
}
