package store

import (
	"context"
	"database/sql"
	"errors"
)

func (s *Store) InstalledPlugin(ctx context.Context, id string) (*InstalledPlugin, error) {
	var plugin InstalledPlugin
	err := s.q().GetContext(ctx, &plugin, s.Rebind(`SELECT * FROM installed_plugins WHERE id=?`), id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &plugin, err
}

func (s *Store) InstalledPluginByServiceTokenID(ctx context.Context, tokenID string) (*InstalledPlugin, error) {
	var plugin InstalledPlugin
	err := s.q().GetContext(ctx, &plugin, s.Rebind(`SELECT * FROM installed_plugins WHERE service_token_id=?`), tokenID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &plugin, err
}

func (s *Store) ListInstalledPlugins(ctx context.Context) ([]InstalledPlugin, error) {
	var plugins []InstalledPlugin
	err := s.q().SelectContext(ctx, &plugins, `SELECT * FROM installed_plugins ORDER BY id`)
	return plugins, err
}

func (s *Store) SetInstalledPlugin(ctx context.Context, plugin *InstalledPlugin) error {
	_, err := s.q().ExecContext(ctx, s.Rebind(`
		INSERT INTO installed_plugins
			(id,version,binary_sha256,sealed_secret,service_token_id,port,status,error,installed_at,updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET
			version=excluded.version,
			binary_sha256=excluded.binary_sha256,
			sealed_secret=excluded.sealed_secret,
			service_token_id=excluded.service_token_id,
			port=excluded.port,
			status=excluded.status,
			error=excluded.error,
			installed_at=excluded.installed_at,
			updated_at=excluded.updated_at`),
		plugin.ID, plugin.Version, plugin.BinarySHA256, plugin.SealedSecret, plugin.ServiceTokenID,
		plugin.Port, plugin.Status, plugin.Error, plugin.InstalledAt, plugin.UpdatedAt)
	return err
}

func (s *Store) UpdateInstalledPluginStatus(ctx context.Context, id, status, message string) error {
	_, err := s.q().ExecContext(ctx, s.Rebind(`
		UPDATE installed_plugins SET status=?, error=?, updated_at=? WHERE id=?`),
		status, message, s.NowMs(), id)
	return err
}

func (s *Store) DeleteInstalledPlugin(ctx context.Context, id string) error {
	_, err := s.q().ExecContext(ctx, s.Rebind(`DELETE FROM installed_plugins WHERE id=?`), id)
	return err
}

func (s *Store) ServiceTokenByID(ctx context.Context, id string) (*ServiceToken, error) {
	var token ServiceToken
	err := s.q().GetContext(ctx, &token, s.Rebind(`SELECT * FROM service_tokens WHERE id=?`), id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &token, err
}

func (s *Store) CreatePluginInstall(ctx context.Context, token *ServiceToken, plugin *InstalledPlugin) error {
	tx, err := s.DB.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var existing ServiceToken
	err = tx.GetContext(ctx, &existing, s.Rebind(`SELECT * FROM service_tokens WHERE name=?`), token.Name)
	switch {
	case err == nil:
		if existing.RevokedAt == nil {
			return ErrConflict
		}
		token.ID = existing.ID
		_, err = tx.ExecContext(ctx, s.Rebind(`
			UPDATE service_tokens
			SET token_hash=?,scopes=?,group_ids=NULL,created_at=?,last_used_at=NULL,revoked_at=NULL
			WHERE id=?`),
			token.TokenHash, token.ScopesJSON, token.CreatedAt, token.ID)
	case errors.Is(err, sql.ErrNoRows):
		_, err = tx.ExecContext(ctx, s.Rebind(`
			INSERT INTO service_tokens (id,name,token_hash,scopes,group_ids,created_at)
			VALUES (?,?,?,?,?,?)`),
			token.ID, token.Name, token.TokenHash, token.ScopesJSON, token.GroupIDsJSON, token.CreatedAt)
	default:
		return err
	}
	if err != nil {
		return err
	}

	plugin.ServiceTokenID = token.ID
	_, err = tx.ExecContext(ctx, s.Rebind(`
		INSERT INTO installed_plugins
			(id,version,binary_sha256,sealed_secret,service_token_id,port,status,error,installed_at,updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET
			version=excluded.version,
			binary_sha256=excluded.binary_sha256,
			sealed_secret=excluded.sealed_secret,
			service_token_id=excluded.service_token_id,
			port=excluded.port,
			status=excluded.status,
			error=excluded.error,
			installed_at=excluded.installed_at,
			updated_at=excluded.updated_at`),
		plugin.ID, plugin.Version, plugin.BinarySHA256, plugin.SealedSecret, plugin.ServiceTokenID,
		plugin.Port, plugin.Status, plugin.Error, plugin.InstalledAt, plugin.UpdatedAt)
	if err != nil {
		return err
	}
	return tx.Commit()
}
