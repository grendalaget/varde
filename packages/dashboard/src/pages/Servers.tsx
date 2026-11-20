import { useEffect, useState } from "react";
import { api, errorMessage } from "../api/client";
import {
  Button,
  Card,
  CardHeader,
  Empty,
  ErrorNote,
  PageHeader,
} from "../components/ui";
import { useLoad } from "../lib/data";
import { useOnEvent } from "../lib/events";
import { navigate } from "../lib/router";
import { canAdmin, useSession } from "../lib/session";
import PluginSettingsPanel from "./PluginSettings";
import { PageLoader } from "../components/Logo";
import { ServerList } from "../components/ServerList";
import { isCrossplayEnabled, useGames } from "../lib/games";
import { Icon } from "../components/icons";

export default function Servers() {
  const { group } = useSession();
  const games = useGames();
  const { data, refresh, loading, error } = useLoad(
    () =>
      api.GET("/v1/groups/{groupId}/servers", {
        params: { path: { groupId: group.group_id } },
      }),
    [group.group_id],
  );
  useOnEvent(
    (e) =>
      !!e.server_id ||
      e.type.startsWith("snapshot.") ||
      e.type.startsWith("node."),
    refresh,
  );
  const servers = data?.servers ?? [];

  return (
    <>
      <PageHeader
        title="Servers"
        subtitle="Each server runs on one machine at a time and is backed up on the others."
        actions={
          <Button variant="primary" onClick={() => navigate("/servers/new")}>
            <Icon name="plus" className="size-3.5" />
            New server
          </Button>
        }
      />
      <PluginSetup />
      {loading && !data && <PageLoader />}
      {error && (
        <ErrorNote>
          {errorMessage(error)}{" "}
          <Button onClick={() => void refresh()}>Try again</Button>
        </ErrorNote>
      )}
      {!loading && !error && servers.length === 0 && (
        <Card className="vd-enter">
          <Empty
            action={
              <Button onClick={() => navigate("/servers/new")}>
                Create your first server
              </Button>
            }
          >
            No servers yet. Pick a game and Varde runs it on one of your group's
            machines.
          </Empty>
        </Card>
      )}
      {servers.length > 0 && (
        <ServerList
          entries={servers.map((s) => ({
            server: s,
            gameName: games.name(s.game_id),
            crossplay: isCrossplayEnabled(games.get(s.game_id), s.config),
          }))}
        />
      )}
    </>
  );
}

function PluginSetup() {
  const { group } = useSession();
  const catalog = useLoad(() => api.GET("/v1/plugins"), []);
  const enabledPlugins = useLoad(
    () =>
      api.GET("/v1/groups/{groupId}/plugins", {
        params: { path: { groupId: group.group_id } },
      }),
    [group.group_id],
  );
  const refreshCatalog = catalog.refresh;
  const refreshEnabled = enabledPlugins.refresh;
  useEffect(() => {
    const refresh = async () => {
      await refreshCatalog();
      await refreshEnabled();
    };
    window.addEventListener("varde:plugins-changed", refresh);
    return () => window.removeEventListener("varde:plugins-changed", refresh);
  }, [refreshCatalog, refreshEnabled]);
  const [setupPluginId, setSetupPluginId] = useState<string | null>(null);

  if (!canAdmin(group.role)) return null;
  if (
    (catalog.loading && !catalog.data) ||
    (enabledPlugins.loading && !enabledPlugins.data)
  )
    return null;
  const enabled = new Set(
    (enabledPlugins.data?.plugins ?? [])
      .filter((plugin) => plugin.enabled)
      .map((plugin) => plugin.id),
  );
  const available = (catalog.data?.plugins ?? []).filter(
    (plugin) => !enabled.has(plugin.id),
  );
  const setupError = [
    catalog.error ? errorMessage(catalog.error) : "",
    enabledPlugins.error ? errorMessage(enabledPlugins.error) : "",
  ]
    .filter(Boolean)
    .join(" ");

  return (
    <section className="mb-6 space-y-4">
      {setupError && <ErrorNote>{setupError}</ErrorNote>}
      {available.length > 0 && (
        <>
          <h2 className="text-base font-semibold text-foreground">
            Set up plugins
          </h2>
          <div className="grid gap-4 md:grid-cols-2">
            {available.map((plugin) => (
              <Card key={plugin.id}>
                <CardHeader
                  title={plugin.name}
                  subtitle={plugin.description || undefined}
                />
                <div className="flex justify-end p-4 pt-0">
                  <Button onClick={() => setSetupPluginId(plugin.id)}>
                    Set up
                  </Button>
                </div>
              </Card>
            ))}
          </div>
        </>
      )}
      {setupPluginId && (
        <PluginSettingsPanel
          pluginId={setupPluginId}
          onClose={() => setSetupPluginId(null)}
        />
      )}
    </section>
  );
}
