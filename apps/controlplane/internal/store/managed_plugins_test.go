package store

import (
	"errors"
	"testing"
)

func TestManagedPluginServerOwnershipIsAtomic(t *testing.T) {
	for name, s := range testStores(t) {
		t.Run(name, func(t *testing.T) {
			t.Cleanup(func() { _ = s.Close() })
			ctx := t.Context()
			mkUser(t, s, "usr_managed")
			group := &Group{ID: "grp_managed", Name: "Managed", SettingsJSON: "{}", CreatedAt: 1}
			if err := s.CreateGroup(ctx, group, "usr_managed"); err != nil {
				t.Fatal(err)
			}
			create := func(id, plugin, ip string) error {
				return s.CreateManagedServer(ctx,
					&Server{ID: id, GroupID: group.ID, Name: "Managed", GameID: "autotournament", DesiredState: "stopped", ObservedState: "stopped", ConfigJSON: "{}", CreatedAt: 1, UpdatedAt: 1},
					&Service{ID: "svc_" + id, ServerID: id, GroupID: group.ID, LoopbackIP: ip, PortsJSON: "[]"},
					&ManagedPluginServer{GroupID: group.ID, PluginID: plugin, SecretsJSON: "encrypted"})
			}
			if err := create("srv_managedone", "managed-plugin", "127.77.2.1"); err != nil {
				t.Fatal(err)
			}
			if err := create("srv_managedtwo", "managed-plugin", "127.77.2.2"); err == nil {
				t.Error("CreateManagedServer(duplicate group/plugin) = nil, want uniqueness failure")
			}
			if _, err := s.GetServer(ctx, "srv_managedtwo"); !errors.Is(err, ErrNotFound) {
				t.Errorf("GetServer(failed duplicate) error = %v, want no persisted server", err)
			}
			if err := create("srv_managedthree", "another-plugin", "127.77.2.2"); err != nil {
				t.Fatalf("CreateManagedServer(other plugin) = %v, want reused address after rollback", err)
			}
			if err := s.Migrate(); err != nil {
				t.Fatal(err)
			}
			row, err := s.ManagedPluginServer(ctx, group.ID, "managed-plugin")
			if err != nil {
				t.Fatal(err)
			}
			if row.ServerID != "srv_managedone" || row.SecretsJSON != "encrypted" {
				t.Errorf("ManagedPluginServer after migration = %v, want original server and encrypted state", row)
			}
		})
	}
}
