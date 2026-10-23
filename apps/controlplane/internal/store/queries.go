package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jmoiron/sqlx"
)

// Querier is satisfied by *sqlx.DB and *sqlx.Tx.
type Querier interface {
	sqlx.ExtContext
	SelectContext(ctx context.Context, dest any, query string, args ...any) error
	GetContext(ctx context.Context, dest any, query string, args ...any) error
}

// rebindOnly is used inside queries that already run on a Querier bound to
// the right dialect — callers pass s.Rebind(q) themselves. For single-shot
// helpers we wrap the store.

var ErrNotFound = errors.New("store: not found")

func (s *Store) q() *sqlx.DB { return s.DB }

// ---- users / sessions ----

func (s *Store) CreateUser(ctx context.Context, u *User) error {
	_, err := s.q().ExecContext(ctx, s.Rebind(
		`INSERT INTO users (id,email,display_name,password_hash,is_operator,website_user_id,created_at)
		 VALUES (?,?,?,?,?,?,?)`),
		u.ID, u.Email, u.DisplayName, u.PasswordHash, u.IsOperator, u.WebsiteUserID, u.CreatedAt)
	return err
}

// CreateUserFirstOperator inserts a user that becomes operator iff the
// users table is empty — decided inside the INSERT, so two concurrent
// sign-ups racing an empty table can't both gain the role. On return,
// u.IsOperator holds the value actually stored.
func (s *Store) CreateUserFirstOperator(ctx context.Context, u *User) error {
	return s.q().GetContext(ctx, &u.IsOperator, s.Rebind(
		`INSERT INTO users (id,email,display_name,password_hash,is_operator,website_user_id,created_at)
		 SELECT ?,?,?,?,
		        CASE WHEN (SELECT COUNT(*) FROM users) = 0 THEN 1 ELSE 0 END,
		        ?,?
		 RETURNING is_operator`),
		u.ID, u.Email, u.DisplayName, u.PasswordHash, u.WebsiteUserID, u.CreatedAt)
}

func (s *Store) UserByEmail(ctx context.Context, email string) (*User, error) {
	var u User
	err := s.q().GetContext(ctx, &u, s.Rebind(`SELECT * FROM users WHERE email=?`), email)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &u, err
}

func (s *Store) UserByID(ctx context.Context, id string) (*User, error) {
	var u User
	err := s.q().GetContext(ctx, &u, s.Rebind(`SELECT * FROM users WHERE id=?`), id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &u, err
}

func (s *Store) UserByWebsiteID(ctx context.Context, websiteUserID string) (*User, error) {
	var u User
	err := s.q().GetContext(ctx, &u, s.Rebind(`SELECT * FROM users WHERE website_user_id=?`), websiteUserID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &u, err
}

// UpdateUserIdentity refreshes a federated user's mirrored profile fields
// (email/display name drift on varde.games side).
func (s *Store) UpdateUserIdentity(ctx context.Context, id, email, displayName string) error {
	_, err := s.q().ExecContext(ctx, s.Rebind(
		`UPDATE users SET email=?, display_name=? WHERE id=?`), email, displayName, id)
	return err
}

// BindWebsiteUserID links an existing account to a varde.games identity,
// refreshing its mirrored profile fields in the same write.
func (s *Store) BindWebsiteUserID(ctx context.Context, id, websiteUserID, email, displayName string) error {
	_, err := s.q().ExecContext(ctx, s.Rebind(
		`UPDATE users SET website_user_id=?, email=?, display_name=? WHERE id=?`), websiteUserID, email, displayName, id)
	return err
}

func (s *Store) UserCount(ctx context.Context) (int64, error) {
	var n int64
	err := s.q().GetContext(ctx, &n, `SELECT COUNT(*) FROM users`)
	return n, err
}

func (s *Store) CreateSession(ctx context.Context, sess *Session) error {
	_, err := s.q().ExecContext(ctx, s.Rebind(
		`INSERT INTO sessions (token_hash,user_id,created_at,expires_at) VALUES (?,?,?,?)`),
		sess.TokenHash, sess.UserID, sess.CreatedAt, sess.ExpiresAt)
	return err
}

func (s *Store) SessionByTokenHash(ctx context.Context, hash string) (*Session, error) {
	var sess Session
	err := s.q().GetContext(ctx, &sess, s.Rebind(`SELECT * FROM sessions WHERE token_hash=?`), hash)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &sess, err
}

func (s *Store) DeleteSession(ctx context.Context, hash string) error {
	_, err := s.q().ExecContext(ctx, s.Rebind(`DELETE FROM sessions WHERE token_hash=?`), hash)
	return err
}

// ---- groups / members / limits ----

func (s *Store) CreateGroup(ctx context.Context, g *Group, ownerUserID string) error {
	return s.Tx(ctx, func(tx *sqlx.Tx) error {
		if _, err := tx.ExecContext(ctx, s.Rebind(
			`INSERT INTO groups (id,name,plan,settings_json,created_at) VALUES (?,?,?,?,?)`),
			g.ID, g.Name, g.Plan, g.SettingsJSON, g.CreatedAt); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, s.Rebind(
			`INSERT INTO group_members (group_id,user_id,role,created_at) VALUES (?,?,'owner',?)`),
			g.ID, ownerUserID, g.CreatedAt)
		return err
	})
}

func (s *Store) GetGroup(ctx context.Context, id string) (*Group, error) {
	var g Group
	err := s.q().GetContext(ctx, &g, s.Rebind(`SELECT * FROM groups WHERE id=?`), id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &g, err
}

func (s *Store) UpdateGroup(ctx context.Context, g *Group) error {
	_, err := s.q().ExecContext(ctx, s.Rebind(
		`UPDATE groups SET name=?, settings_json=? WHERE id=?`),
		g.Name, g.SettingsJSON, g.ID)
	return err
}

func (s *Store) DeleteGroup(ctx context.Context, id string) error {
	_, err := s.q().ExecContext(ctx, s.Rebind(`DELETE FROM groups WHERE id=?`), id)
	return err
}

func (s *Store) GroupsForUser(ctx context.Context, userID string) ([]Member, error) {
	var ms []Member
	err := s.q().SelectContext(ctx, &ms, s.Rebind(
		`SELECT m.group_id, m.user_id, m.role, m.created_at, u.email, u.display_name
		 FROM group_members m JOIN users u ON u.id=m.user_id WHERE m.user_id=?`), userID)
	return ms, err
}

func (s *Store) MemberRole(ctx context.Context, groupID, userID string) (string, error) {
	var role string
	err := s.q().GetContext(ctx, &role, s.Rebind(
		`SELECT role FROM group_members WHERE group_id=? AND user_id=?`), groupID, userID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return role, err
}

func (s *Store) ListMembers(ctx context.Context, groupID string) ([]Member, error) {
	var ms []Member
	err := s.q().SelectContext(ctx, &ms, s.Rebind(
		`SELECT m.group_id, m.user_id, m.role, m.created_at, u.email, u.display_name
		 FROM group_members m JOIN users u ON u.id=m.user_id WHERE m.group_id=?`), groupID)
	return ms, err
}

func (s *Store) SetMemberRole(ctx context.Context, groupID, userID, role string) error {
	res, err := s.q().ExecContext(ctx, s.Rebind(
		`UPDATE group_members SET role=? WHERE group_id=? AND user_id=?`), role, groupID, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) AddMember(ctx context.Context, groupID, userID, role string, now int64) error {
	_, err := s.q().ExecContext(ctx, s.Rebind(
		`INSERT INTO group_members (group_id,user_id,role,created_at) VALUES (?,?,?,?)`),
		groupID, userID, role, now)
	return err
}

func (s *Store) RemoveMember(ctx context.Context, groupID, userID string) error {
	res, err := s.q().ExecContext(ctx, s.Rebind(
		`DELETE FROM group_members WHERE group_id=? AND user_id=?`), groupID, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) AllGroups(ctx context.Context) ([]Group, error) {
	var gs []Group
	err := s.q().SelectContext(ctx, &gs, `SELECT * FROM groups ORDER BY created_at`)
	return gs, err
}

func (s *Store) GetLimits(ctx context.Context, groupID string) (*GroupLimits, error) {
	var l GroupLimits
	err := s.q().GetContext(ctx, &l, s.Rebind(`SELECT * FROM group_limits WHERE group_id=?`), groupID)
	if errors.Is(err, sql.ErrNoRows) {
		return &GroupLimits{GroupID: groupID}, nil // unlimited
	}
	return &l, err
}

func (s *Store) SetLimits(ctx context.Context, l *GroupLimits) error {
	_, err := s.q().ExecContext(ctx, s.Rebind(
		`INSERT INTO group_limits (group_id,max_nodes,max_servers,max_storage_bytes,max_relay_bytes_month)
		 VALUES (?,?,?,?,?)
		 ON CONFLICT(group_id) DO UPDATE SET max_nodes=excluded.max_nodes,
		   max_servers=excluded.max_servers, max_storage_bytes=excluded.max_storage_bytes,
		   max_relay_bytes_month=excluded.max_relay_bytes_month`),
		l.GroupID, l.MaxNodes, l.MaxServers, l.MaxStorageBytes, l.MaxRelayBytesMon)
	return err
}

func (s *Store) NodeCount(ctx context.Context, groupID string) (int64, error) {
	var n int64
	err := s.q().GetContext(ctx, &n, s.Rebind(`SELECT COUNT(*) FROM nodes WHERE group_id=?`), groupID)
	return n, err
}

func (s *Store) ServerCount(ctx context.Context, groupID string) (int64, error) {
	var n int64
	err := s.q().GetContext(ctx, &n, s.Rebind(`SELECT COUNT(*) FROM servers WHERE group_id=?`), groupID)
	return n, err
}

// ---- invites ----

func (s *Store) CreateInvite(ctx context.Context, inv *Invite) error {
	_, err := s.q().ExecContext(ctx, s.Rebind(
		`INSERT INTO invites (code,group_id,role,created_by,expires_at,max_uses,uses)
		 VALUES (?,?,?,?,?,?,0)`),
		inv.Code, inv.GroupID, inv.Role, inv.CreatedBy, inv.ExpiresAt, inv.MaxUses)
	return err
}

func (s *Store) GetInvite(ctx context.Context, code string) (*Invite, error) {
	var inv Invite
	err := s.q().GetContext(ctx, &inv, s.Rebind(`SELECT * FROM invites WHERE code=?`), code)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &inv, err
}

func (s *Store) ConsumeInvite(ctx context.Context, code string) error {
	res, err := s.q().ExecContext(ctx, s.Rebind(
		`UPDATE invites SET uses=uses+1 WHERE code=? AND (max_uses IS NULL OR uses<max_uses)
		 AND (expires_at IS NULL OR expires_at>?)`), code, s.NowMs())
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("invite expired or exhausted")
	}
	return nil
}

// RefundInvite returns one use — consumed but unneeded when a concurrent
// accept had already committed the membership.
func (s *Store) RefundInvite(ctx context.Context, code string) error {
	_, err := s.q().ExecContext(ctx, s.Rebind(
		`UPDATE invites SET uses=uses-1 WHERE code=? AND uses>0`), code)
	return err
}

// ---- enrollment tokens / device links ----

func (s *Store) CreateEnrollmentToken(ctx context.Context, t *EnrollmentToken) error {
	_, err := s.q().ExecContext(ctx, s.Rebind(
		`INSERT INTO enrollment_tokens (id,group_id,token_hash,anchor,hosting_enabled,labels_json,expires_at,max_uses,uses,created_by)
		 VALUES (?,?,?,?,?,?,?,?,0,?)`),
		t.ID, t.GroupID, t.TokenHash, t.Anchor, t.HostingEnabled, t.LabelsJSON, t.ExpiresAt, t.MaxUses, t.CreatedBy)
	return err
}

func (s *Store) EnrollmentTokenByHash(ctx context.Context, hash string) (*EnrollmentToken, error) {
	var t EnrollmentToken
	err := s.q().GetContext(ctx, &t, s.Rebind(`SELECT * FROM enrollment_tokens WHERE token_hash=?`), hash)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &t, err
}

func (s *Store) ListEnrollmentTokens(ctx context.Context, groupID string) ([]EnrollmentToken, error) {
	var ts []EnrollmentToken
	err := s.q().SelectContext(ctx, &ts, s.Rebind(
		`SELECT * FROM enrollment_tokens WHERE group_id=? ORDER BY id`), groupID)
	return ts, err
}

func (s *Store) DeleteEnrollmentToken(ctx context.Context, groupID, id string) error {
	res, err := s.q().ExecContext(ctx, s.Rebind(
		`DELETE FROM enrollment_tokens WHERE group_id=? AND id=?`), groupID, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) ConsumeEnrollmentToken(ctx context.Context, hash string) (*EnrollmentToken, error) {
	err := s.Tx(ctx, func(tx *sqlx.Tx) error {
		res, err := tx.ExecContext(ctx, s.Rebind(
			`UPDATE enrollment_tokens SET uses=uses+1 WHERE token_hash=?
			 AND (expires_at IS NULL OR expires_at>?) AND (max_uses IS NULL OR uses<max_uses)`),
			hash, s.NowMs())
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("token expired or exhausted")
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.EnrollmentTokenByHash(ctx, hash)
}

// ---- service tokens ----

func (s *Store) CreateServiceToken(ctx context.Context, t *ServiceToken) error {
	_, err := s.q().ExecContext(ctx, s.Rebind(
		`INSERT INTO service_tokens (id,name,token_hash,scopes,group_ids,created_at) VALUES (?,?,?,?,?,?)`),
		t.ID, t.Name, t.TokenHash, t.ScopesJSON, t.GroupIDsJSON, t.CreatedAt)
	return err
}

func (s *Store) ServiceTokenByHash(ctx context.Context, hash string) (*ServiceToken, error) {
	var t ServiceToken
	err := s.q().GetContext(ctx, &t, s.Rebind(`SELECT * FROM service_tokens WHERE token_hash=?`), hash)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &t, err
}

func (s *Store) ListServiceTokens(ctx context.Context) ([]ServiceToken, error) {
	var ts []ServiceToken
	err := s.q().SelectContext(ctx, &ts, `SELECT * FROM service_tokens ORDER BY created_at`)
	return ts, err
}

func (s *Store) RevokeServiceToken(ctx context.Context, id string) error {
	res, err := s.q().ExecContext(ctx, s.Rebind(
		`UPDATE service_tokens SET revoked_at=? WHERE id=? AND revoked_at IS NULL`), s.NowMs(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// TouchServiceToken stamps last_used_at; best-effort from the auth path.
func (s *Store) TouchServiceToken(ctx context.Context, id string) {
	_, _ = s.q().ExecContext(ctx, s.Rebind(`UPDATE service_tokens SET last_used_at=? WHERE id=?`), s.NowMs(), id)
}

func (s *Store) CreateDeviceLink(ctx context.Context, d *DeviceLink) error {
	_, err := s.q().ExecContext(ctx, s.Rebind(
		`INSERT INTO device_links (user_code,device_code_hash,public_key,hostname,os,arch,agent_version,state,expires_at,created_at)
		 VALUES (?,?,?,?,?,?,?,'pending',?,?)`),
		d.UserCode, d.DeviceCodeHash, d.PublicKey, d.Hostname, d.OS, d.Arch, d.AgentVersion,
		d.ExpiresAt, d.CreatedAt)
	return err
}

func (s *Store) DeviceLinkByUserCode(ctx context.Context, code string) (*DeviceLink, error) {
	var d DeviceLink
	err := s.q().GetContext(ctx, &d, s.Rebind(`SELECT * FROM device_links WHERE user_code=?`), code)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &d, err
}

func (s *Store) DeviceLinkByDeviceHash(ctx context.Context, hash string) (*DeviceLink, error) {
	var d DeviceLink
	err := s.q().GetContext(ctx, &d, s.Rebind(`SELECT * FROM device_links WHERE device_code_hash=?`), hash)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &d, err
}

func (s *Store) UpdateDeviceLink(ctx context.Context, d *DeviceLink) error {
	_, err := s.q().ExecContext(ctx, s.Rebind(
		`UPDATE device_links SET state=?, group_id=?, node_id=?, approved_by=? WHERE user_code=?`),
		d.State, d.GroupID, d.NodeID, d.ApprovedBy, d.UserCode)
	return err
}

// ---- nodes ----

func (s *Store) CreateNode(ctx context.Context, n *Node) error {
	_, err := s.q().ExecContext(ctx, s.Rebind(
		`INSERT INTO nodes (id,group_id,name,public_key,os,arch,agent_version,hosting_enabled,anchor,priority,
		                    max_memory_mb,max_cpu_percent,max_storage_bytes,admin_state,owner_user_id,last_seen_at,created_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`),
		n.ID, n.GroupID, n.Name, n.PublicKey, n.OS, n.Arch, n.AgentVersion, n.HostingEnabled,
		n.Anchor, n.Priority, n.MaxMemoryMB, n.MaxCPUPercent, n.MaxStorageBytes, n.AdminState,
		n.OwnerUserID, n.LastSeenAt, n.CreatedAt)
	return err
}

func (s *Store) GetNode(ctx context.Context, id string) (*Node, error) {
	var n Node
	err := s.q().GetContext(ctx, &n, s.Rebind(`SELECT * FROM nodes WHERE id=?`), id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &n, err
}

func (s *Store) NodeByPubKey(ctx context.Context, pubKeyB64 string) (*Node, error) {
	var n Node
	err := s.q().GetContext(ctx, &n, s.Rebind(`SELECT * FROM nodes WHERE public_key=?`), pubKeyB64)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &n, err
}

func (s *Store) ListNodes(ctx context.Context, groupID string) ([]Node, error) {
	var ns []Node
	err := s.q().SelectContext(ctx, &ns, s.Rebind(
		`SELECT * FROM nodes WHERE group_id=? ORDER BY created_at`), groupID)
	return ns, err
}

func (s *Store) UpdateNode(ctx context.Context, n *Node) error {
	_, err := s.q().ExecContext(ctx, s.Rebind(
		`UPDATE nodes SET name=?, hosting_enabled=?, anchor=?, priority=?, max_memory_mb=?,
		 max_cpu_percent=?, max_storage_bytes=?, admin_state=? WHERE id=?`),
		n.Name, n.HostingEnabled, n.Anchor, n.Priority, n.MaxMemoryMB, n.MaxCPUPercent,
		n.MaxStorageBytes, n.AdminState, n.ID)
	return err
}

func (s *Store) DeleteNode(ctx context.Context, id string) error {
	_, err := s.q().ExecContext(ctx, s.Rebind(`DELETE FROM nodes WHERE id=?`), id)
	return err
}

func (s *Store) TouchNodeSeen(ctx context.Context, nodeID string, now int64) error {
	_, err := s.q().ExecContext(ctx, s.Rebind(
		`UPDATE nodes SET last_seen_at=? WHERE id=?`), now, nodeID)
	return err
}

func (s *Store) PutNodeStatus(ctx context.Context, st *NodeStatus) error {
	_, err := s.q().ExecContext(ctx, s.Rebind(
		`INSERT INTO node_status (node_id,status_json,mesh_json,updated_at) VALUES (?,?,?,?)
		 ON CONFLICT(node_id) DO UPDATE SET status_json=excluded.status_json,
		   mesh_json=excluded.mesh_json, updated_at=excluded.updated_at`),
		st.NodeID, st.StatusJSON, st.MeshJSON, st.UpdatedAt)
	return err
}

func (s *Store) GetNodeStatus(ctx context.Context, nodeID string) (*NodeStatus, error) {
	var st NodeStatus
	err := s.q().GetContext(ctx, &st, s.Rebind(`SELECT * FROM node_status WHERE node_id=?`), nodeID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &st, err
}

// ---- servers / services ----

func (s *Store) CreateServer(ctx context.Context, srv *Server, svc *Service) error {
	return s.Tx(ctx, func(tx *sqlx.Tx) error {
		if _, err := tx.ExecContext(ctx, s.Rebind(
			`INSERT INTO servers (id,group_id,name,game_id,deployment_id,desired_state,observed_state,
			  replication_factor,min_commit_replicas,snapshot_interval_s,preferred_node_id,epoch,
			  config_json,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`),
			srv.ID, srv.GroupID, srv.Name, srv.GameID, srv.DeploymentID, srv.DesiredState,
			srv.ObservedState, srv.ReplicationFactor, srv.MinCommitReplicas, srv.SnapshotIntervalS,
			srv.PreferredNodeID, srv.Epoch, srv.ConfigJSON, srv.CreatedAt, srv.UpdatedAt); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, s.Rebind(
			`INSERT INTO services (id,server_id,group_id,loopback_ip,ports_json) VALUES (?,?,?,?,?)`),
			svc.ID, svc.ServerID, svc.GroupID, svc.LoopbackIP, svc.PortsJSON)
		return err
	})
}

func (s *Store) GetServer(ctx context.Context, id string) (*Server, error) {
	var srv Server
	err := s.q().GetContext(ctx, &srv, s.Rebind(`SELECT * FROM servers WHERE id=?`), id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &srv, err
}

func (s *Store) ListServers(ctx context.Context, groupID string) ([]Server, error) {
	var ss []Server
	err := s.q().SelectContext(ctx, &ss, s.Rebind(
		`SELECT * FROM servers WHERE group_id=? ORDER BY created_at`), groupID)
	return ss, err
}

func (s *Store) AllServers(ctx context.Context) ([]Server, error) {
	var ss []Server
	err := s.q().SelectContext(ctx, &ss, `SELECT * FROM servers ORDER BY id`)
	return ss, err
}

func (s *Store) UpdateServerDesired(ctx context.Context, id, desired string, now int64) error {
	_, err := s.q().ExecContext(ctx, s.Rebind(
		`UPDATE servers SET desired_state=?, updated_at=? WHERE id=?`), desired, now, id)
	return err
}

func (s *Store) UpdateServerMeta(ctx context.Context, srv *Server) error {
	_, err := s.q().ExecContext(ctx, s.Rebind(
		`UPDATE servers SET name=?, config_json=?, replication_factor=?, min_commit_replicas=?,
		 snapshot_interval_s=?, preferred_node_id=?, observed_state=?, updated_at=? WHERE id=?`),
		srv.Name, srv.ConfigJSON, srv.ReplicationFactor, srv.MinCommitReplicas,
		srv.SnapshotIntervalS, srv.PreferredNodeID, srv.ObservedState, srv.UpdatedAt, srv.ID)
	return err
}

func (s *Store) SetObservedState(ctx context.Context, q Querier, id, state string, now int64) error {
	_, err := q.ExecContext(ctx, s.Rebind(
		`UPDATE servers SET observed_state=?, updated_at=? WHERE id=?`), state, now, id)
	return err
}

func (s *Store) DeleteServer(ctx context.Context, id string) error {
	_, err := s.q().ExecContext(ctx, s.Rebind(`DELETE FROM servers WHERE id=?`), id)
	return err
}

func (s *Store) ServiceByServer(ctx context.Context, serverID string) (*Service, error) {
	var svc Service
	err := s.q().GetContext(ctx, &svc, s.Rebind(`SELECT * FROM services WHERE server_id=?`), serverID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &svc, err
}

func (s *Store) ServicesInGroup(ctx context.Context, groupID string) ([]Service, error) {
	var svcs []Service
	err := s.q().SelectContext(ctx, &svcs, s.Rebind(
		`SELECT * FROM services WHERE group_id=?`), groupID)
	return svcs, err
}

// BumpEpochAndCreateExecution performs the single-transaction activation
// from the spec: optimistic epoch bump + insert of the new active execution.
// The unique partial index enforces I1 against races.
func (s *Store) BumpEpochAndCreateExecution(ctx context.Context, e *Execution) error {
	return s.Tx(ctx, func(tx *sqlx.Tx) error {
		res, err := tx.ExecContext(ctx, s.Rebind(
			`UPDATE servers SET epoch=epoch+1, updated_at=? WHERE id=? AND epoch=?`),
			s.NowMs(), e.ServerID, e.Epoch-1)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("epoch bump failed (concurrent activation)")
		}
		role := e.Role
		if role == "" {
			role = "serving"
		}
		_, err = tx.ExecContext(ctx, s.Rebind(
			`INSERT INTO server_executions (id,server_id,node_id,epoch,state,action,stop_reason,
			  lease_expires_at,restore_snapshot_id,placement_json,health,message,created_at,started_at,ended_at,end_reason,role)
			 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`),
			e.ID, e.ServerID, e.NodeID, e.Epoch, e.State, e.Action, e.StopReason,
			e.LeaseExpiresAt, e.RestoreSnapshotID, e.PlacementJSON, e.Health, e.Message,
			e.CreatedAt, e.StartedAt, e.EndedAt, e.EndReason, role)
		return err
	})
}

func (s *Store) GetExecution(ctx context.Context, id string) (*Execution, error) {
	var e Execution
	err := s.q().GetContext(ctx, &e, s.Rebind(`SELECT * FROM server_executions WHERE id=?`), id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &e, err
}

func (s *Store) SetExecutionJoinCode(ctx context.Context, tx *sqlx.Tx, executionID, joinCode string, now int64) (bool, error) {
	var current sql.NullString
	err := tx.GetContext(ctx, &current, s.Rebind(
		`SELECT join_code FROM execution_info WHERE execution_id=?`), executionID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	if joinCode == "" {
		if err != nil || !current.Valid || current.String == "" {
			return false, nil
		}
		_, err = tx.ExecContext(ctx, s.Rebind(
			`UPDATE execution_info SET join_code=NULL,updated_at=? WHERE execution_id=?`),
			now, executionID)
		return false, err
	}
	if err == nil && current.Valid && current.String == joinCode {
		return false, nil
	}
	_, err = tx.ExecContext(ctx, s.Rebind(
		`INSERT INTO execution_info (execution_id,join_code,updated_at) VALUES (?,?,?)
		 ON CONFLICT(execution_id) DO UPDATE SET join_code=excluded.join_code,updated_at=excluded.updated_at`),
		executionID, joinCode, now)
	return err == nil, err
}

func (s *Store) ExecutionJoinCode(ctx context.Context, executionID string) (*string, error) {
	var code sql.NullString
	err := s.q().GetContext(ctx, &code, s.Rebind(
		`SELECT join_code FROM execution_info WHERE execution_id=?`), executionID)
	if err != nil {
		return nil, err
	}
	if !code.Valid {
		return nil, nil
	}
	if code.String == "" {
		return nil, nil
	}
	return &code.String, nil
}

func (s *Store) ActiveExecution(ctx context.Context, serverID string) (*Execution, error) {
	var e Execution
	err := s.q().GetContext(ctx, &e, s.Rebind(
		`SELECT * FROM server_executions WHERE server_id=? AND ended_at IS NULL AND role='serving'`), serverID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &e, err
}

func (s *Store) ActiveExecutionsForNode(ctx context.Context, nodeID string) ([]Execution, error) {
	var es []Execution
	err := s.q().SelectContext(ctx, &es, s.Rebind(
		`SELECT * FROM server_executions WHERE node_id=? AND ended_at IS NULL`), nodeID)
	return es, err
}

func (s *Store) ExecutionsForServer(ctx context.Context, serverID string) ([]Execution, error) {
	var es []Execution
	err := s.q().SelectContext(ctx, &es, s.Rebind(
		`SELECT * FROM server_executions WHERE server_id=? ORDER BY created_at DESC`), serverID)
	return es, err
}

func (s *Store) UpdateExecutionState(ctx context.Context, q Querier, e *Execution) error {
	_, err := q.ExecContext(ctx, s.Rebind(
		`UPDATE server_executions SET state=?, action=?, stop_reason=?, lease_expires_at=?,
		 health=?, message=?, started_at=?, ended_at=?, end_reason=?,
		 role=COALESCE(NULLIF(?,''),role) WHERE id=?`),
		e.State, e.Action, e.StopReason, e.LeaseExpiresAt, e.Health, e.Message,
		e.StartedAt, e.EndedAt, e.EndReason, e.Role, e.ID)
	return err
}

func (s *Store) EndExecution(ctx context.Context, q Querier, id string, endReason string, now int64) error {
	_, err := q.ExecContext(ctx, s.Rebind(
		`UPDATE server_executions SET ended_at=?, end_reason=? WHERE id=? AND ended_at IS NULL`),
		now, endReason, id)
	return err
}

func (s *Store) RenewLease(ctx context.Context, id string, expiresAt int64) error {
	_, err := s.q().ExecContext(ctx, s.Rebind(
		`UPDATE server_executions SET lease_expires_at=? WHERE id=? AND ended_at IS NULL`),
		expiresAt, id)
	return err
}

// ---- migrations ----

func (s *Store) CreateMigration(ctx context.Context, m *Migration) error {
	_, err := s.q().ExecContext(ctx, s.Rebind(
		`INSERT INTO migrations (id,server_id,from_execution_id,to_node_id,state,mode,created_at)
		 VALUES (?,?,?,?,?,?,?)`),
		m.ID, m.ServerID, m.FromExecutionID, m.ToNodeID, m.State, m.Mode, m.CreatedAt)
	return err
}

func (s *Store) OpenMigrations(ctx context.Context) ([]Migration, error) {
	var ms []Migration
	err := s.q().SelectContext(ctx, &ms, s.Rebind(
		`SELECT * FROM migrations WHERE finished_at IS NULL`))
	return ms, err
}

// DrainingMigration returns the open warm migration for a server whose drain
// window is still live, used to attach drain fields to routes.
func (s *Store) DrainingMigration(ctx context.Context, serverID string, now int64) (*Migration, error) {
	var m Migration
	err := s.q().GetContext(ctx, &m, s.Rebind(
		`SELECT * FROM migrations WHERE server_id=? AND drain_until IS NOT NULL AND drain_until>?`),
		serverID, now)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &m, err
}

func (s *Store) UpdateMigration(ctx context.Context, m *Migration) error {
	_, err := s.q().ExecContext(ctx, s.Rebind(
		`UPDATE migrations SET state=?, final_snapshot_id=?, finished_at=?,
		 to_execution_id=COALESCE(?,to_execution_id), drain_until=COALESCE(?,drain_until) WHERE id=?`),
		m.State, m.FinalSnapshotID, m.FinishedAt, m.ToExecutionID, m.DrainUntil, m.ID)
	return err
}

// ---- snapshots ----

func (s *Store) CreateSnapshot(ctx context.Context, snap *Snapshot) error {
	return s.CreateSnapshotTx(ctx, s.q(), snap)
}

// CreateSnapshotTx inserts inside a caller-managed transaction.
func (s *Store) CreateSnapshotTx(ctx context.Context, q Querier, snap *Snapshot) error {
	_, err := q.ExecContext(ctx, s.Rebind(
		`INSERT INTO snapshots (id,server_id,group_id,epoch,execution_id,node_id,parent_id,
		 manifest_digest,deployment_id,reason,state,size_bytes,stored_bytes,file_count,chunk_count,
		 pinned,created_at,committed_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`),
		snap.ID, snap.ServerID, snap.GroupID, snap.Epoch, snap.ExecutionID, snap.NodeID,
		snap.ParentID, snap.ManifestDigest, snap.DeploymentID, snap.Reason, snap.State,
		snap.SizeBytes, snap.StoredBytes, snap.FileCount, snap.ChunkCount, snap.Pinned,
		snap.CreatedAt, snap.CommittedAt)
	return err
}

func (s *Store) GetSnapshot(ctx context.Context, id string) (*Snapshot, error) {
	var snap Snapshot
	err := s.q().GetContext(ctx, &snap, s.Rebind(`SELECT * FROM snapshots WHERE id=?`), id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &snap, err
}

func (s *Store) SnapshotsForServer(ctx context.Context, serverID string) ([]Snapshot, error) {
	var snaps []Snapshot
	err := s.q().SelectContext(ctx, &snaps, s.Rebind(
		`SELECT * FROM snapshots WHERE server_id=? ORDER BY created_at`), serverID)
	return snaps, err
}

// LatestCommittedSnapshotAt returns created_at of the server's newest
// committed snapshot ("Latest safe save"), or nil when it has none.
func (s *Store) LatestCommittedSnapshotAt(ctx context.Context, serverID string) (*int64, error) {
	var at sql.NullInt64
	err := s.q().GetContext(ctx, &at, s.Rebind(
		`SELECT MAX(created_at) FROM snapshots WHERE server_id=? AND state='committed'`), serverID)
	if err != nil || !at.Valid {
		return nil, err
	}
	return &at.Int64, nil
}

// LatestSnapshot returns the server's newest snapshot of any non-invalid
// state, or nil when it has none.
func (s *Store) LatestSnapshot(ctx context.Context, serverID string) (*Snapshot, error) {
	var snaps []Snapshot
	err := s.q().SelectContext(ctx, &snaps, s.Rebind(
		`SELECT * FROM snapshots WHERE server_id=? AND state!='invalid'
		 ORDER BY created_at DESC LIMIT 1`), serverID)
	if err != nil || len(snaps) == 0 {
		return nil, err
	}
	return &snaps[0], nil
}

// LatestCommittedSnapshot returns the server's newest committed snapshot
// ("Latest safe save"), or nil when it has none.
func (s *Store) LatestCommittedSnapshot(ctx context.Context, serverID string) (*Snapshot, error) {
	var snaps []Snapshot
	err := s.q().SelectContext(ctx, &snaps, s.Rebind(
		`SELECT * FROM snapshots WHERE server_id=? AND state='committed'
		 ORDER BY created_at DESC LIMIT 1`), serverID)
	if err != nil || len(snaps) == 0 {
		return nil, err
	}
	return &snaps[0], nil
}

func (s *Store) SnapshotsForGroup(ctx context.Context, groupID string) ([]Snapshot, error) {
	var snaps []Snapshot
	err := s.q().SelectContext(ctx, &snaps, s.Rebind(
		`SELECT * FROM snapshots WHERE group_id=? ORDER BY created_at`), groupID)
	return snaps, err
}

func (s *Store) SetSnapshotState(ctx context.Context, q Querier, id, state string, committedAt *int64) error {
	_, err := q.ExecContext(ctx, s.Rebind(
		`UPDATE snapshots SET state=?, committed_at=COALESCE(?,committed_at) WHERE id=?`),
		state, committedAt, id)
	return err
}

func (s *Store) UpdateSnapshotPinned(ctx context.Context, id string, pinned bool) error {
	res, err := s.q().ExecContext(ctx, s.Rebind(
		`UPDATE snapshots SET pinned=? WHERE id=?`), Bool(pinned), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ---- replicas ----

func (s *Store) UpsertReplica(ctx context.Context, q Querier, r *Replica) error {
	_, err := q.ExecContext(ctx, s.Rebind(
		`INSERT INTO snapshot_replicas (snapshot_id,node_id,state,assigned_at,ready_at)
		 VALUES (?,?,?,?,?)
		 ON CONFLICT(snapshot_id,node_id) DO UPDATE SET state=excluded.state, ready_at=excluded.ready_at`),
		r.SnapshotID, r.NodeID, r.State, r.AssignedAt, r.ReadyAt)
	return err
}

func (s *Store) ReplicasForSnapshot(ctx context.Context, snapID string) ([]Replica, error) {
	var rs []Replica
	err := s.q().SelectContext(ctx, &rs, s.Rebind(
		`SELECT * FROM snapshot_replicas WHERE snapshot_id=?`), snapID)
	return rs, err
}

func (s *Store) AssignedReplicasForNode(ctx context.Context, nodeID string) ([]Replica, error) {
	var rs []Replica
	err := s.q().SelectContext(ctx, &rs, s.Rebind(
		`SELECT * FROM snapshot_replicas WHERE node_id=? AND state='assigned'`), nodeID)
	return rs, err
}

func (s *Store) DeletingReplicasForNode(ctx context.Context, nodeID string) ([]Replica, error) {
	var rs []Replica
	err := s.q().SelectContext(ctx, &rs, s.Rebind(
		`SELECT * FROM snapshot_replicas WHERE node_id=? AND state='deleting'`), nodeID)
	return rs, err
}

func (s *Store) DeleteReplica(ctx context.Context, q Querier, snapID, nodeID string) error {
	_, err := q.ExecContext(ctx, s.Rebind(
		`DELETE FROM snapshot_replicas WHERE snapshot_id=? AND node_id=?`), snapID, nodeID)
	return err
}

// ---- snapshot requests ----

func (s *Store) CreateSnapshotRequest(ctx context.Context, r *SnapshotRequest) error {
	_, err := s.q().ExecContext(ctx, s.Rebind(
		`INSERT INTO snapshot_requests (id,server_id,reason,created_at,claimed) VALUES (?,?,?,?,0)`),
		r.ID, r.ServerID, r.Reason, r.CreatedAt)
	return err
}

func (s *Store) PendingSnapshotRequests(ctx context.Context, serverID string) ([]SnapshotRequest, error) {
	var rs []SnapshotRequest
	err := s.q().SelectContext(ctx, &rs, s.Rebind(
		`SELECT * FROM snapshot_requests WHERE server_id=? AND claimed=0 ORDER BY created_at`), serverID)
	return rs, err
}

func (s *Store) ClaimSnapshotRequest(ctx context.Context, id string) error {
	_, err := s.q().ExecContext(ctx, s.Rebind(
		`UPDATE snapshot_requests SET claimed=1 WHERE id=?`), id)
	return err
}

func (s *Store) SnapshotRequestByID(ctx context.Context, id string) (*SnapshotRequest, error) {
	var r SnapshotRequest
	err := s.q().GetContext(ctx, &r, s.Rebind(`SELECT * FROM snapshot_requests WHERE id=?`), id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &r, err
}

// ---- events / audit / logs ----

// EmitEvent appends an event inside the caller's transaction (or the DB).
func (s *Store) EmitEvent(ctx context.Context, q Querier, groupID string, serverID, nodeID *string, typ string, data any) error {
	b, _ := json.Marshal(data)
	_, err := q.ExecContext(ctx, s.Rebind(
		`INSERT INTO events (group_id,server_id,node_id,type,data_json,created_at) VALUES (?,?,?,?,?,?)`),
		groupID, serverID, nodeID, typ, string(b), s.NowMs())
	return err
}

func (s *Store) EventsAfter(ctx context.Context, groupID string, after int64, limit int) ([]Event, error) {
	if limit <= 0 {
		limit = 200
	}
	var evs []Event
	err := s.q().SelectContext(ctx, &evs, s.Rebind(
		`SELECT * FROM events WHERE group_id=? AND id>? ORDER BY id LIMIT ?`), groupID, after, limit)
	return evs, err
}

func (s *Store) Audit(ctx context.Context, q Querier, groupID *string, actorType, actorID, action, target string, data any) error {
	b, _ := json.Marshal(data)
	_, err := q.ExecContext(ctx, s.Rebind(
		`INSERT INTO audit_log (group_id,actor_type,actor_id,action,target,data_json,created_at)
		 VALUES (?,?,?,?,?,?,?)`),
		groupID, actorType, actorID, action, target, string(b), s.NowMs())
	return err
}

func (s *Store) AppendLogLines(ctx context.Context, execID string, lines []LogLineRow) error {
	return s.Tx(ctx, func(tx *sqlx.Tx) error {
		for _, l := range lines {
			if _, err := tx.ExecContext(ctx, s.Rebind(
				`INSERT INTO execution_logs (execution_id,seq,at,stream,line)
				 VALUES (?,COALESCE((SELECT MAX(seq) FROM execution_logs WHERE execution_id=?),0)+1,?,?,?)`),
				execID, execID, l.At, l.Stream, l.Line); err != nil {
				return err
			}
		}
		// cap to last 2000 lines
		_, err := tx.ExecContext(ctx, s.Rebind(
			`DELETE FROM execution_logs WHERE execution_id=? AND seq <=
			 (SELECT COALESCE(MAX(seq),0)-2000 FROM execution_logs WHERE execution_id=?)`),
			execID, execID)
		return err
	})
}

func (s *Store) LogsForExecution(ctx context.Context, execID string) ([]LogLineRow, error) {
	var ls []LogLineRow
	err := s.q().SelectContext(ctx, &ls, s.Rebind(
		`SELECT * FROM execution_logs WHERE execution_id=? ORDER BY seq`), execID)
	return ls, err
}

// ---- kv ----

func (s *Store) KVGet(ctx context.Context, key string) (string, error) {
	var v string
	err := s.q().GetContext(ctx, &v, s.Rebind(`SELECT value FROM kv WHERE key=?`), key)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return v, err
}

func (s *Store) KVSet(ctx context.Context, key, value string) error {
	_, err := s.q().ExecContext(ctx, s.Rebind(
		`INSERT INTO kv (key,value) VALUES (?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`),
		key, value)
	return err
}

// KVSetNX inserts key/value only when key is absent, reporting whether the
// insert happened. Multi-instance callers racing to seed a shared value use
// this to decide who won — the losers must re-read the stored value instead
// of keeping their own candidate.
func (s *Store) KVSetNX(ctx context.Context, key, value string) (bool, error) {
	res, err := s.q().ExecContext(ctx, s.Rebind(
		`INSERT INTO kv (key,value) VALUES (?,?) ON CONFLICT(key) DO NOTHING`),
		key, value)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// ---- crash reports ----

// InsertCrashReport stores the report and returns its id. When the uploader
// retries with the same (node_id, client_ref) the insert dedupes on the
// unique index and the existing report's id comes back instead — a lost ack
// does not create a second report.
func (s *Store) InsertCrashReport(ctx context.Context, r *CrashReport) (string, error) {
	q := `INSERT INTO crash_reports (id, group_id, node_id, kind, exe, version, os, arch, pid,
			message, location, backtrace, detail, created_at, received_at, dump, dump_name, dump_omitted, client_ref)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`
	if s.Dialect == "sqlite" {
		q = "INSERT OR IGNORE" + q[6:]
	} else {
		q += " ON CONFLICT (node_id, client_ref) WHERE client_ref != '' DO NOTHING"
	}
	if _, err := s.DB.ExecContext(ctx, s.Rebind(q),
		r.ID, r.GroupID, r.NodeID, r.Kind, r.Exe, r.Version, r.Os, r.Arch, r.Pid,
		r.Message, r.Location, r.Backtrace, r.Detail, r.CreatedAt, r.ReceivedAt,
		r.Dump, r.DumpName, r.DumpOmitted, r.ClientRef); err != nil {
		return "", err
	}
	if r.ClientRef == "" {
		return r.ID, nil
	}
	var id string
	if err := s.DB.GetContext(ctx, &id, s.Rebind(
		`SELECT id FROM crash_reports WHERE node_id = ? AND client_ref = ?`),
		r.NodeID, r.ClientRef); err != nil {
		return "", err
	}
	return id, nil
}

// ListCrashReports returns the newest `limit` reports for the group (no dumps),
// with the node's display name joined in.
func (s *Store) ListCrashReports(ctx context.Context, groupID string, limit int) ([]CrashReportMeta, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	var out []CrashReportMeta
	err := s.DB.SelectContext(ctx, &out, s.Rebind(
		`SELECT c.id, c.node_id, COALESCE(n.name, '') AS node_name, c.kind, c.exe, c.version,
			c.message, c.created_at, c.received_at,
			COALESCE(LENGTH(c.dump), 0) AS dump_size, c.dump_omitted
		 FROM crash_reports c LEFT JOIN nodes n ON n.id = c.node_id
		 WHERE c.group_id = ? ORDER BY c.created_at DESC LIMIT ?`), groupID, limit)
	return out, err
}

// GetCrashReport returns the full report including the dump, or nil.
func (s *Store) GetCrashReport(ctx context.Context, groupID, id string) (*CrashReport, error) {
	var r CrashReport
	err := s.DB.GetContext(ctx, &r, s.Rebind(
		`SELECT * FROM crash_reports WHERE group_id = ? AND id = ?`), groupID, id)
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// DirectiveFingerprintData returns the compact state used to detect
// reconciler-driven changes to agent directives.
func (s *Store) DirectiveFingerprintData(ctx context.Context) (DirectiveFingerprintData, error) {
	var data DirectiveFingerprintData
	if err := s.DB.SelectContext(ctx, &data.Groups, `SELECT id FROM groups ORDER BY id`); err != nil {
		return data, err
	}
	if err := s.DB.SelectContext(ctx, &data.Executions, s.Rebind(
		`SELECT sv.group_id, e.id, e.node_id, e.state, e.action, e.epoch, e.role, e.stop_reason
		 FROM server_executions e
		 JOIN servers sv ON sv.id = e.server_id
		 WHERE e.ended_at IS NULL
		 ORDER BY sv.group_id, e.id`)); err != nil {
		return data, err
	}
	if err := s.DB.SelectContext(ctx, &data.Migrations, s.Rebind(
		`SELECT sv.group_id, m.id, m.server_id, m.from_execution_id, m.to_node_id, m.state,
		        m.mode, m.to_execution_id, m.drain_until, m.final_snapshot_id, m.created_at, m.finished_at
		 FROM migrations m
		 JOIN servers sv ON sv.id = m.server_id
		 WHERE m.finished_at IS NULL OR m.drain_until > ?
		 ORDER BY sv.group_id, m.id`), s.NowMs()); err != nil {
		return data, err
	}
	if err := s.DB.SelectContext(ctx, &data.Nodes, s.Rebind(
		`SELECT id, group_id, admin_state, hosting_enabled, anchor, last_seen_at
		 FROM nodes ORDER BY group_id, id`)); err != nil {
		return data, err
	}
	if err := s.DB.SelectContext(ctx, &data.Replicas, s.Rebind(
		`SELECT sn.group_id, r.snapshot_id, r.node_id, r.state
		 FROM snapshot_replicas r
		 JOIN snapshots sn ON sn.id = r.snapshot_id
		 WHERE r.state IN ('assigned', 'ready', 'deleting')
		 ORDER BY sn.group_id, r.snapshot_id, r.node_id`)); err != nil {
		return data, err
	}
	if err := s.DB.SelectContext(ctx, &data.Events, s.Rebind(
		`SELECT group_id, COALESCE(MAX(id), 0) AS max_id
		 FROM events GROUP BY group_id ORDER BY group_id`)); err != nil {
		return data, err
	}
	if err := s.DB.SelectContext(ctx, &data.Servers, s.Rebind(
		`SELECT group_id, id, desired_state, observed_state, epoch
		 FROM servers ORDER BY group_id, id`)); err != nil {
		return data, err
	}
	return data, nil
}
