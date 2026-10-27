import { useEffect, useState } from "react";
import { api, errorMessage, type Server } from "../api/client";
import {
  Badge,
  Button,
  Card,
  CardHeader,
  CopyText,
  Empty,
  ErrorNote,
  PageHeader,
  ServerStateBadge,
} from "../components/ui";
import { useLoad } from "../lib/data";
import { useOnEvent } from "../lib/events";
import { timeAgo } from "../lib/format";
import { Link, navigate } from "../lib/router";
import { canAdmin, useSession } from "../lib/session";
import PluginSettingsPanel from "./PluginSettings";
import SaveSafety from "../components/SaveSafety";
import { HostMarker, ServerMark } from "../components/Logo";
import { isCrossplayEnabled, useGames } from "../lib/games";

export default function Servers() {
  const { group } = useSession();
  const games = useGames();
  const { data, refresh, loading } = useLoad(
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
            New server
          </Button>
        }
      />
      <PluginSetup />
      {!loading && servers.length === 0 && (
        <Card className="vd-enter">
          <Empty>
            No servers yet.{" "}
            <Link
              to="/servers/new"
              className="text-foreground underline decoration-muted underline-offset-2 hover:decoration-foreground"
            >
              Create your first one
            </Link>
            .
          </Empty>
        </Card>
      )}
      <div className="vd-stagger grid gap-4 md:grid-cols-2">
        {servers.map((s) => (
          <ServerCard
            key={s.id}
            s={s}
            gameName={games.name(s.game_id)}
            crossplay={isCrossplayEnabled(games.get(s.game_id), s.config)}
          />
        ))}
      </div>
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
  const refreshEnabled = enabledPlugins.refresh;
  useEffect(() => {
    const refresh = () => void refreshEnabled();
    window.addEventListener("varde:plugins-changed", refresh);
    return () => window.removeEventListener("varde:plugins-changed", refresh);
  }, [refreshEnabled]);
  const [setupPluginId, setSetupPluginId] = useState<string | null>(null);

  if (!canAdmin(group.role)) return null;
  if (catalog.loading || enabledPlugins.loading) return null;
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
  ].filter(Boolean).join(" ");

  return (
    <section className="mb-6 space-y-4">
      {setupError && <ErrorNote>{setupError}</ErrorNote>}
      {available.length > 0 && (
        <>
          <h2 className="text-base font-semibold text-foreground">Set up plugins</h2>
          <div className="grid gap-4 md:grid-cols-2">
            {available.map((plugin) => (
              <Card key={plugin.id}>
                <CardHeader
                  title={plugin.name}
                  subtitle={plugin.description || undefined}
                />
                <div className="flex justify-end p-4 pt-0">
                  <Button
                    variant="primary"
                    onClick={() => setSetupPluginId(plugin.id)}
                  >
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

function ServerCard({
  s,
  gameName,
  crossplay,
}: {
  s: Server;
  gameName: string;
  crossplay: boolean;
}) {
  const sum = s.summary;
  return (
    <Link
      to={`/servers/${s.id}`}
      className="block rounded-2xl border border-separator bg-surface p-4 transition-all duration-200 ease-vd-out hover:-translate-y-0.5 hover:border-field-border hover:shadow-lg hover:shadow-black/40 focus-visible:outline focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-foreground"
    >
      <div className="flex items-start justify-between gap-3">
        <div className="flex min-w-0 items-center gap-3">
          <ServerMark state={s.observed_state} />
          <div className="min-w-0">
            <h3 className="truncate font-semibold text-foreground">{s.name}</h3>
            <p className="text-xs text-muted">{gameName}</p>
          </div>
        </div>
        <ServerStateBadge state={s.observed_state} />
      </div>
      <dl className="mt-4 space-y-2 text-sm">
        <Row label="Hosting on">
          {sum.hosting_on ? (
            <HostMarker
              nodeId={sum.hosting_on.node_id}
              name={sum.hosting_on.name}
            />
          ) : (
            <span className="text-muted">Not running</span>
          )}
        </Row>
        <Row label={crossplay ? "Join code" : "Address"}>
          {crossplay ? (
            sum.join_code ? (
              <span
                onClick={(e) => {
                  e.preventDefault();
                  e.stopPropagation();
                }}
              >
                <CopyText text={sum.join_code} />
              </span>
            ) : (
              <span className="text-muted">
                {s.observed_state === "running" ? "Waiting for code…" : "-"}
              </span>
            )
          ) : sum.address ? (
            <span
              onClick={(e) => {
                e.preventDefault();
                e.stopPropagation();
              }}
            >
              <CopyText text={sum.address} />
            </span>
          ) : (
            <span className="text-muted">-</span>
          )}
        </Row>
        {s.game_id === "cs2" && (
          <p className="text-xs text-muted">
            In CS2 open the console and run{" "}
            <code>connect {sum.address || "<address>"}</code>.
          </p>
        )}
        {crossplay && (
          <p className="text-xs text-muted">
            Crossplay server: players join from Valheim&apos;s Join game → Join
            by code. The code can change when the server restarts or moves to
            another machine, so check here for the current one.
          </p>
        )}
        <Row label="Latest safe save">
          {sum.latest_safe_save ? (
            <span className="text-foreground">
              {timeAgo(sum.latest_safe_save.created_at)}
            </span>
          ) : (
            <span className="text-muted">None yet</span>
          )}
        </Row>
      </dl>
      <div className="mt-3">
        <SaveSafety summary={sum} compact />
      </div>
      {s.desired_state === "running" && s.observed_state === "failed" && (
        <div className="mt-2">
          <Badge tone="red">Needs attention</Badge>
        </div>
      )}
    </Link>
  );
}

function Row({
  label,
  children,
}: {
  label: string;
  children: React.ReactNode;
}) {
  return (
    <div className="flex items-center justify-between gap-3">
      <dt className="text-muted">{label}</dt>
      <dd className="min-w-0 truncate text-right">{children}</dd>
    </div>
  );
}
