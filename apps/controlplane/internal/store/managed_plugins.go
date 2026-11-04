package store

import (
	"context"

	"github.com/jmoiron/sqlx"
)

type ManagedPluginServer struct {
	GroupID       string `db:"group_id"`
	PluginID      string `db:"plugin_id"`
	ServerID      string `db:"server_id"`
	SecretsJSON   string `db:"secrets_json"`
	BindingDigest string `db:"binding_digest"`
	Attempts      int64  `db:"attempts"`
	RetryAt       int64  `db:"retry_at"`
	BindingError  string `db:"binding_error"`
}

func (s *Store) ManagedPluginServer(ctx context.Context, groupID, pluginID string) (*ManagedPluginServer, error) {
	var row ManagedPluginServer
	err := s.q().GetContext(ctx, &row, s.Rebind(`SELECT * FROM managed_plugin_servers WHERE group_id=? AND plugin_id=?`), groupID, pluginID)
	return &row, err
}

func (s *Store) ManagedPluginServerByServer(ctx context.Context, serverID string) (*ManagedPluginServer, error) {
	var row ManagedPluginServer
	err := s.q().GetContext(ctx, &row, s.Rebind(`SELECT * FROM managed_plugin_servers WHERE server_id=?`), serverID)
	return &row, err
}

func (s *Store) ManagedPluginServers(ctx context.Context) ([]ManagedPluginServer, error) {
	var rows []ManagedPluginServer
	err := s.q().SelectContext(ctx, &rows, `SELECT * FROM managed_plugin_servers ORDER BY group_id,plugin_id`)
	return rows, err
}

func (s *Store) UpdateManagedPluginBinding(ctx context.Context, row *ManagedPluginServer) error {
	_, err := s.DB.ExecContext(ctx, s.Rebind(`UPDATE managed_plugin_servers SET binding_digest=?,attempts=?,retry_at=?,binding_error=? WHERE group_id=? AND plugin_id=? AND server_id=?`),
		row.BindingDigest, row.Attempts, row.RetryAt, row.BindingError, row.GroupID, row.PluginID, row.ServerID)
	return err
}

func (s *Store) CompareAndSwapGroupPluginSettings(ctx context.Context, previous, group *GroupPlugin) (bool, error) {
	return s.compareAndSwapGroupPluginSettings(ctx, s.q(), previous, group)
}

func (s *Store) compareAndSwapGroupPluginSettings(ctx context.Context, q Querier, previous, group *GroupPlugin) (bool, error) {
	res, err := q.ExecContext(ctx, s.Rebind(`UPDATE group_plugins SET settings_json=?,updated_at=? WHERE group_id=? AND plugin_id=? AND settings_json=? AND updated_at=? AND enabled_by=? AND created_at=?`), group.SettingsJSON, group.UpdatedAt, previous.GroupID, previous.PluginID, previous.SettingsJSON, previous.UpdatedAt, previous.EnabledBy, previous.CreatedAt)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func (s *Store) CommitManagedPluginBinding(ctx context.Context, row *ManagedPluginServer, previous, group *GroupPlugin) error {
	return s.Tx(ctx, func(tx *sqlx.Tx) error {
		if updated, err := s.compareAndSwapGroupPluginSettings(ctx, tx, previous, group); err != nil {
			return err
		} else if !updated {
			return ErrConflict
		}
		_, err := tx.ExecContext(ctx, s.Rebind(`UPDATE managed_plugin_servers SET binding_digest=?,attempts=?,retry_at=?,binding_error=? WHERE group_id=? AND plugin_id=? AND server_id=?`),
			row.BindingDigest, row.Attempts, row.RetryAt, row.BindingError, row.GroupID, row.PluginID, row.ServerID)
		return err
	})
}
