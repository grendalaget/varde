import { useEffect, useRef, useState } from "react";
import {
  api,
  errorMessage,
  type Execution,
  type Server,
  type Snapshot,
} from "../api/client";
import {
  Badge,
  Button,
  Card,
  CardHeader,
  ChoiceCard,
  CopyText,
  cx,
  Dot,
  Empty,
  ErrorNote,
  Field,
  inputClass,
  ServerStateBadge,
  WarnNote,
} from "../components/ui";
import { HostMarker, PageLoader } from "../components/Logo";
import { GameMark } from "../components/GameMark";
import { Icon } from "../components/icons";
import { useLoad } from "../lib/data";
import { useOnEvent } from "../lib/events";
import { bytes, dateTime, timeAgo } from "../lib/format";
import { Link, navigate } from "../lib/router";
import { canAdmin, useNodeName, useSession } from "../lib/session";
import EventList from "../components/EventList";
import { useMergedEvents } from "../lib/activity";
import { ConfigInput } from "../components/ConfigInput";
import { isWideField } from "../lib/configField";
import SaveSafety from "../components/SaveSafety";
import {
  askedFields,
  gameBackdrop,
  isCrossplayEnabled,
  useGames,
  withServerName,
} from "../lib/games";

type Tab = "overview" | "saves" | "logs" | "settings" | "details";

export default function ServerDetail({ id }: { id: string }) {
  const [tab, setTab] = useState<Tab>("overview");
  const server = useLoad(
    () =>
      api.GET("/v1/servers/{serverId}", { params: { path: { serverId: id } } }),
    [id],
  );
  const snaps = useLoad(
    () =>
      api.GET("/v1/servers/{serverId}/snapshots", {
        params: { path: { serverId: id } },
      }),
    [id],
  );
  const snapshots = [...(snaps.data?.snapshots ?? [])].sort(
    (a, b) => b.created_at - a.created_at,
  );
  const execs = useLoad(
    () =>
      api.GET("/v1/servers/{serverId}/executions", {
        params: { path: { serverId: id } },
      }),
    [id],
  );
  useOnEvent(
    (e) => e.server_id === id || e.type.startsWith("node."),
    () => {
      void server.refresh();
      void snaps.refresh();
      void execs.refresh();
    },
  );
  const games = useGames();

  if (server.error && !server.data) {
    return (
      <ErrorNote>
        Server not found.{" "}
        <Link to="/" className="underline">
          Back to servers
        </Link>
      </ErrorNote>
    );
  }
  const s = server.data;
  if (!s) return <PageLoader />;

  const tabs: [Tab, string][] = [
    ["overview", "Overview"],
    ["saves", "Saves"],
    ["logs", "Logs"],
    ["settings", "Settings"],
    ["details", "Details"],
  ];

  return (
    <>
      <div className="mb-3 text-sm">
        <Link
          to="/"
          className="inline-flex items-center gap-1.5 text-muted transition-colors hover:text-foreground"
        >
          <Icon name="back" className="size-3.5" />
          Servers
        </Link>
      </div>
      <header className="vd-enter relative mb-5 overflow-hidden rounded-2xl border border-separator bg-surface shadow-surface">
        <div
          aria-hidden
          className="absolute inset-0"
          style={{ background: gameBackdrop(s.game_id) }}
        />
        <div className="relative flex flex-wrap items-center justify-between gap-4 p-5 sm:p-6">
          <div className="flex min-w-0 items-center gap-4">
            <GameMark id={s.game_id} name={games.name(s.game_id)} size="lg" />
            <div className="min-w-0">
              <div className="flex flex-wrap items-center gap-x-3 gap-y-1">
                <h1 className="truncate font-display text-[1.75rem] leading-tight font-semibold tracking-tight text-foreground">
                  {s.name}
                </h1>
                <ServerStateBadge state={s.observed_state} />
              </div>
              <p className="mt-0.5 text-sm text-muted">
                {games.name(s.game_id)}
              </p>
            </div>
          </div>
          <Actions
            s={s}
            onChange={() => {
              void server.refresh();
              void execs.refresh();
            }}
          />
        </div>
      </header>
      <div
        role="tablist"
        className="mb-6 flex w-fit max-w-full gap-1 overflow-x-auto rounded-full border border-separator bg-surface p-1 [scrollbar-width:none]"
      >
        {tabs.map(([t, label]) => (
          <button
            key={t}
            role="tab"
            aria-selected={tab === t}
            onClick={() => setTab(t)}
            className={cx(
              "shrink-0 rounded-full px-3.5 py-1.5 text-sm transition-colors duration-200 ease-vd-out",
              tab === t
                ? "bg-default font-medium text-foreground shadow-[inset_0_1px_0_rgb(255_255_255/0.05)]"
                : "text-muted hover:text-foreground",
            )}
          >
            {label}
          </button>
        ))}
      </div>
      <div key={tab} className="vd-in">
        {tab === "overview" && (
          <Overview
            s={s}
            snapshots={snapshots}
            crossplay={isCrossplayEnabled(games.get(s.game_id), s.config)}
          />
        )}
        {tab === "saves" && (
          <Saves s={s} snapshots={snapshots} refresh={snaps.refresh} />
        )}
        {tab === "logs" && (
          <Logs s={s} executions={execs.data?.executions ?? []} />
        )}
        {tab === "settings" && (
          <ServerSettings s={s} onSaved={server.refresh} />
        )}
        {tab === "details" && (
          <Details s={s} executions={execs.data?.executions ?? []} />
        )}
      </div>
    </>
  );
}

function Actions({ s, onChange }: { s: Server; onChange: () => void }) {
  const { group, nodes } = useSession();
  const [busy, setBusy] = useState<string | null>(null);
  const [err, setErr] = useState<{ msg: string; code?: string } | null>(null);
  const [moving, setMoving] = useState(false);
  const running = s.desired_state === "running";
  const hostId = s.summary.hosting_on?.node_id;
  const targets = nodes.filter(
    (n) =>
      n.hosting_enabled &&
      n.admin_state === "active" &&
      n.online &&
      n.id !== hostId,
  );
  // a start can't place anywhere: offer to add a machine (or turn on hosting)
  const noHosts =
    err?.code === "unschedulable" &&
    !nodes.some((n) => n.hosting_enabled && n.admin_state === "active");

  async function run(name: string, fn: () => Promise<{ error?: unknown }>) {
    setBusy(name);
    setErr(null);
    const { error } = await fn();
    setBusy(null);
    if (error) {
      const code = (error as { code?: string }).code;
      setErr({ msg: errorMessage(error), code });
    }
    onChange();
  }

  const start = (allowOlder = false) =>
    run("start", () =>
      api.POST("/v1/servers/{serverId}/start", {
        params: { path: { serverId: s.id } },
        body: allowOlder ? { allow_older_snapshot: true } : {},
      }),
    );

  return (
    <div className="relative flex flex-col items-end gap-2">
      <div className="flex flex-wrap justify-end gap-2">
        {running ? (
          <>
            <Button
              busy={busy === "save"}
              disabled={s.observed_state !== "running"}
              onClick={() =>
                run("save", () =>
                  api.POST("/v1/servers/{serverId}/snapshots", {
                    params: { path: { serverId: s.id } },
                  }),
                )
              }
            >
              Save now
            </Button>
            <Button
              disabled={s.observed_state !== "running" || targets.length === 0}
              onClick={() => setMoving(!moving)}
            >
              Move…
            </Button>
            <Button
              variant="danger"
              busy={busy === "stop"}
              onClick={() =>
                run("stop", () =>
                  api.POST("/v1/servers/{serverId}/stop", {
                    params: { path: { serverId: s.id } },
                  }),
                )
              }
            >
              Stop
            </Button>
          </>
        ) : (
          <Button
            variant="primary"
            busy={busy === "start"}
            onClick={() => void start()}
          >
            Start
          </Button>
        )}
      </div>
      {moving && (
        <div className="vd-menu absolute right-0 top-full z-10 mt-2 flex items-center gap-2 rounded-md border border-border bg-surface p-2 text-sm shadow-lg">
          <span className="text-muted">Move to</span>
          {targets.map((n) => (
            <Button
              key={n.id}
              busy={busy === "move:" + n.id}
              onClick={async () => {
                await run("move:" + n.id, () =>
                  api.POST("/v1/servers/{serverId}/move", {
                    params: { path: { serverId: s.id } },
                    body: { target_node_id: n.id },
                  }),
                );
                setMoving(false);
              }}
            >
              {n.name}
            </Button>
          ))}
        </div>
      )}
      {err && (
        <div className="max-w-md space-y-2 text-left">
          <ErrorNote>
            {err.code === "latest_save_unavailable"
              ? "The latest save is on a machine that's offline right now. Wait for it to come back, or start from the newest save that's available (recent progress may be lost)."
              : err.code === "no_reachable_save"
                ? "No save is reachable right now. Wait for the machine holding it to come back online."
                : noHosts && nodes.length === 0
                  ? `${group.name} has no machines yet. Add this PC (or another machine) to run the server?`
                  : noHosts
                    ? `None of ${group.name}'s machines are allowed to host servers. Turn on hosting for one of them?`
                    : err.msg}
          </ErrorNote>
          {noHosts && (
            <Button
              variant="primary"
              onClick={() =>
                navigate(nodes.length === 0 ? "/machines/add" : "/machines")
              }
            >
              {nodes.length === 0 ? "Add a machine" : "Open machines"}
            </Button>
          )}
          {err.code === "latest_save_unavailable" && (
            <Button variant="danger" onClick={() => void start(true)}>
              Start from an older save
            </Button>
          )}
        </div>
      )}
    </div>
  );
}

function Overview({
  s,
  snapshots,
  crossplay,
}: {
  s: Server;
  snapshots: Snapshot[];
  crossplay: boolean;
}) {
  const sum = s.summary;
  const events = useMergedEvents(
    (e) => e.server_id === s.id && e.type !== "execution.state",
  );
  const latestSafe = snapshots.find(
    (x) => x.id === sum.latest_safe_save?.snapshot_id,
  );
  return (
    <div className="vd-stagger grid gap-6 lg:grid-cols-3">
      <div className="vd-stagger space-y-6 lg:col-span-2">
        <Card>
          <CardHeader title="Connect" />
          <div className="space-y-3 p-4 text-sm">
            {s.observed_state === "failed" && (
              <WarnNote>
                The game stopped unexpectedly. Check the logs, then press Stop
                and Start to try again.
              </WarnNote>
            )}
            <div className="flex items-center justify-between gap-3">
              <span className="text-muted">
                {crossplay ? "Join code" : "Address"}
              </span>
              {crossplay ? (
                sum.join_code ? (
                  <CopyText text={sum.join_code} />
                ) : (
                  <span className="text-muted">
                    {s.observed_state === "running" ? "Waiting for code…" : "-"}
                  </span>
                )
              ) : sum.address ? (
                <CopyText text={sum.address} />
              ) : (
                <span className="text-muted">-</span>
              )}
            </div>
            {s.game_id === "cs2" && (
              <p className="text-xs text-muted">
                In CS2 open the console and run{" "}
                <code>connect {sum.address || "<address>"}</code>.
              </p>
            )}
            {crossplay ? (
              <p className="text-xs text-muted">
                Crossplay server: players join from Valheim&apos;s Join game →
                Join by code. The code can change when the server restarts or
                moves to another machine, so check here for the current one.
              </p>
            ) : (
              <p className="text-xs text-muted">
                This address works from any machine in the group with Varde
                running, and it stays the same when the server moves to another
                machine.
              </p>
            )}
            <div className="flex items-center justify-between gap-3">
              <span className="text-muted">Hosting on</span>
              {sum.hosting_on ? (
                <HostMarker
                  nodeId={sum.hosting_on.node_id}
                  name={sum.hosting_on.name}
                />
              ) : (
                <span className="text-muted">Not running</span>
              )}
            </div>
          </div>
        </Card>
        <Card>
          <CardHeader title="Recent activity" />
          <EventList events={events.slice(0, 15)} />
        </Card>
      </div>
      <Card className="h-fit">
        <CardHeader title="Latest safe save" />
        <div className="space-y-3 p-4 text-sm">
          {sum.latest_safe_save ? (
            <>
              <div className="text-foreground">
                {timeAgo(sum.latest_safe_save.created_at)}
                <span className="block text-xs text-muted">
                  {dateTime(sum.latest_safe_save.created_at)}
                </span>
              </div>
              {latestSafe?.size_bytes != null && (
                <div className="text-xs text-muted">
                  {bytes(latestSafe.size_bytes)} of game data
                </div>
              )}
            </>
          ) : (
            <p className="text-muted">
              No safe save yet. One is taken automatically while the server
              runs.
            </p>
          )}
          <SaveSafety summary={sum} />
        </div>
      </Card>
    </div>
  );
}

const SNAP_STATE: Record<
  string,
  { label: string; tone: "green" | "warn" | "neutral" | "red" | "blue" }
> = {
  committed: { label: "Safe", tone: "green" },
  replicating: { label: "Backing up", tone: "blue" },
  local: { label: "Only on host", tone: "warn" },
  superseded: { label: "Older", tone: "neutral" },
  invalid: { label: "Invalid", tone: "red" },
};

const REASON: Record<string, string> = {
  scheduled: "Automatic",
  manual: "Manual",
  final: "On stop",
  migration: "Move",
};

function Saves({
  s,
  snapshots,
  refresh,
}: {
  s: Server;
  snapshots: Snapshot[];
  refresh: () => Promise<void>;
}) {
  const nodeName = useNodeName();
  const { nodes } = useSession();
  const anchorIds = new Set(nodes.filter((n) => n.anchor).map((n) => n.id));
  return (
    <Card>
      <CardHeader
        title="Saves"
        subtitle={`A save is safe once ${s.min_commit_replicas} ${s.min_commit_replicas === 1 ? "machine holds" : "machines hold"} it. Pinned saves are never cleaned up.`}
      />
      {snapshots.length === 0 ? (
        <Empty>No saves yet.</Empty>
      ) : (
        <div className="overflow-x-auto">
          <table className="w-full text-sm">
            <thead className="text-left text-xs uppercase tracking-wider text-muted">
              <tr>
                <th className="px-4 py-2 font-medium">Taken</th>
                <th className="hidden px-4 py-2 font-medium sm:table-cell">
                  Status
                </th>
                <th className="px-4 py-2 font-medium">Backed up on</th>
                <th className="hidden px-4 py-2 font-medium sm:table-cell">
                  Size
                </th>
                <th className="px-4 py-2 font-medium" />
              </tr>
            </thead>
            <tbody className="vd-stagger divide-y divide-separator">
              {snapshots.map((x) => {
                const st = SNAP_STATE[x.state] ?? {
                  label: x.state,
                  tone: "neutral" as const,
                };
                return (
                  <tr key={x.id}>
                    <td className="px-4 py-2">
                      <div
                        className="text-foreground"
                        title={dateTime(x.created_at)}
                      >
                        {timeAgo(x.created_at)}
                      </div>
                      <div className="text-xs text-muted">
                        {REASON[x.reason] ?? x.reason} · on{" "}
                        {nodeName(x.node_id)}
                      </div>
                      <div className="mt-1 flex items-center gap-2 sm:hidden">
                        <Badge tone={st.tone}>{st.label}</Badge>
                        <span className="text-xs text-muted">
                          {bytes(x.size_bytes)}
                        </span>
                      </div>
                    </td>
                    <td className="hidden px-4 py-2 sm:table-cell">
                      <Badge tone={st.tone}>{st.label}</Badge>
                    </td>
                    <td className="px-4 py-2">
                      <div className="flex flex-wrap gap-1">
                        {x.replicas
                          .filter((r) => r.state === "ready")
                          .map((r) => (
                            <Badge
                              key={r.node_id}
                              tone={
                                anchorIds.has(r.node_id) ? "violet" : "neutral"
                              }
                            >
                              {nodeName(r.node_id)}
                            </Badge>
                          ))}
                        {x.replicas.some((r) => r.state === "assigned") && (
                          <span className="text-xs text-muted">
                            +
                            {
                              x.replicas.filter((r) => r.state === "assigned")
                                .length
                            }{" "}
                            copying
                          </span>
                        )}
                      </div>
                    </td>
                    <td className="hidden px-4 py-2 text-muted sm:table-cell">
                      {bytes(x.size_bytes)}
                    </td>
                    <td className="px-4 py-2 text-right">
                      <Button
                        variant="ghost"
                        title={x.pinned ? "Unpin" : "Keep this save forever"}
                        onClick={async () => {
                          await api.PATCH("/v1/snapshots/{snapshotId}", {
                            params: { path: { snapshotId: x.id } },
                            body: { pinned: !x.pinned },
                          });
                          await refresh();
                        }}
                      >
                        {x.pinned ? "Pinned" : "Pin"}
                      </Button>
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      )}
    </Card>
  );
}

function Logs({ s, executions }: { s: Server; executions: Execution[] }) {
  const nodeName = useNodeName();
  const [execId, setExecId] = useState<string>("");
  const effective = execId || executions[0]?.id || "";
  const logs = useLoad(
    () =>
      api.GET("/v1/servers/{serverId}/logs", {
        params: {
          path: { serverId: s.id },
          query: effective ? { execution_id: effective } : {},
        },
      }),
    [s.id, effective],
  );
  const live = !!executions.find((e) => e.id === effective && !e.ended_at);
  const refreshLogs = logs.refresh;
  useEffect(() => {
    if (!live) return;
    const t = setInterval(() => void refreshLogs(), 3000);
    return () => clearInterval(t);
  }, [live, refreshLogs]);
  const box = useRef<HTMLPreElement>(null);
  useEffect(() => {
    box.current?.scrollTo({ top: box.current.scrollHeight });
  }, [logs.data]);

  return (
    <Card>
      <CardHeader
        title="Logs"
        actions={
          executions.length > 0 && (
            <select
              className="max-w-48 rounded-md border border-field-border bg-field px-2 py-1 text-sm transition-colors duration-150 sm:max-w-none"
              value={effective}
              onChange={(e) => setExecId(e.target.value)}
            >
              {executions.map((e) => (
                <option key={e.id} value={e.id}>
                  {nodeName(e.node_id)} ·{" "}
                  {e.started_at ? dateTime(e.started_at) : "not started"}
                  {e.ended_at ? "" : " (current)"}
                </option>
              ))}
            </select>
          )
        }
      />
      <pre
        ref={box}
        className="max-h-[32rem] overflow-auto p-4 font-mono text-xs leading-relaxed text-foreground"
      >
        {(logs.data?.lines ?? []).length === 0
          ? "No log lines."
          : logs.data!.lines.map((l, i) => (
              <div
                key={i}
                className={
                  l.stream === "stderr"
                    ? "text-rose-300"
                    : l.stream === "agent"
                      ? "text-sky-300"
                      : ""
                }
              >
                {l.line}
              </div>
            ))}
      </pre>
    </Card>
  );
}

function ServerSettings({
  s,
  onSaved,
}: {
  s: Server;
  onSaved: () => Promise<void>;
}) {
  const { group, nodes } = useSession();
  const games = useGames();
  const game = games.get(s.game_id);
  const admin = canAdmin(group.role);
  const [name, setName] = useState(s.name);
  const [config, setConfig] = useState<Record<string, unknown>>(s.config ?? {});
  const [rf, setRf] = useState(s.replication_factor);
  const [mcr, setMcr] = useState(s.min_commit_replicas);
  const [interval, setIntervalS] = useState(s.snapshot_interval_s);
  const [preferred, setPreferred] = useState(s.preferred_node_id ?? "");
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [msg, setMsg] = useState<string | null>(null);

  return (
    <form
      className="vd-stagger space-y-6"
      onSubmit={async (e) => {
        e.preventDefault();
        setBusy(true);
        setErr(null);
        setMsg(null);
        const { error } = await api.PATCH("/v1/servers/{serverId}", {
          params: { path: { serverId: s.id } },
          body: {
            name,
            config: withServerName(game, config, name),
            replication_factor: rf,
            min_commit_replicas: mcr,
            snapshot_interval_s: interval,
            preferred_node_id: preferred,
          },
        });
        setBusy(false);
        if (error) setErr(errorMessage(error));
        else {
          setMsg(
            s.desired_state === "running"
              ? "Saved. Game settings apply on next start."
              : "Saved",
          );
          await onSaved();
        }
      }}
    >
      <Card>
        <CardHeader title="Game" />
        <div className="grid gap-x-5 gap-y-5 p-4 sm:grid-cols-2">
          <div className="sm:col-span-2">
            <Field
              label="Server name"
              help={
                askedFields(game).length < (game?.config_fields.length ?? 0)
                  ? "Shown in Varde and in the game's server list."
                  : undefined
              }
            >
              <input
                className={inputClass}
                disabled={!admin}
                value={name}
                onChange={(e) => setName(e.target.value)}
              />
            </Field>
          </div>
          {askedFields(game).map((f) => (
            <div
              key={f.name}
              className={isWideField(f) ? "sm:col-span-2" : undefined}
            >
              <ConfigInput
                f={f}
                value={config[f.name]}
                onChange={(v) => setConfig((c) => ({ ...c, [f.name]: v }))}
              />
            </div>
          ))}
        </div>
      </Card>
      <Card>
        <CardHeader title="Hosting & saves" />
        <div className="grid gap-4 p-4 sm:grid-cols-2">
          <div className="sm:col-span-2">
            <Field
              group
              label="Preferred machine"
              help="Varde prefers this machine when it's online, and uses another one otherwise."
            >
              <div
                role="radiogroup"
                aria-label="Preferred machine"
                className="grid gap-2.5 sm:grid-cols-2"
              >
                <ChoiceCard
                  selected={preferred === ""}
                  onSelect={() => setPreferred("")}
                  title="Any machine"
                  sub="Recommended"
                />
                {nodes
                  .filter((n) => n.hosting_enabled)
                  .map((n) => (
                    <ChoiceCard
                      key={n.id}
                      selected={preferred === n.id}
                      onSelect={() => setPreferred(n.id)}
                      title={n.name}
                      sub={
                        <>
                          <Dot tone={n.online ? "green" : "neutral"} />
                          {n.online ? "Online" : "Offline"}
                        </>
                      }
                    />
                  ))}
              </div>
            </Field>
          </div>
          <Field label="Save every (seconds)">
            <input
              type="number"
              min={30}
              className={inputClass}
              value={interval}
              onChange={(e) => setIntervalS(Number(e.target.value))}
            />
          </Field>
          <Field label="Copies per save">
            <input
              type="number"
              min={1}
              className={inputClass}
              value={rf}
              onChange={(e) => setRf(Number(e.target.value))}
            />
          </Field>
          <Field label="Copies before a save counts as safe">
            <input
              type="number"
              min={1}
              className={inputClass}
              value={mcr}
              onChange={(e) => setMcr(Number(e.target.value))}
            />
          </Field>
        </div>
      </Card>
      <ErrorNote>{err}</ErrorNote>
      <div className="flex items-center justify-between">
        {admin ? (
          <Button
            variant="danger"
            type="button"
            onClick={async () => {
              if (
                prompt(
                  `Type the server name (${s.name}) to delete it and its saves`,
                ) !== s.name
              )
                return;
              const { error } = await api.DELETE("/v1/servers/{serverId}", {
                params: { path: { serverId: s.id } },
              });
              if (error) setErr(errorMessage(error));
              else navigate("/");
            }}
          >
            Delete server
          </Button>
        ) : (
          <span />
        )}
        <div className="flex items-center gap-3">
          {msg && (
            <span className="vd-fade text-sm text-emerald-400">{msg}</span>
          )}
          <Button variant="primary" type="submit" busy={busy}>
            Save
          </Button>
        </div>
      </div>
    </form>
  );
}

function Details({ s, executions }: { s: Server; executions: Execution[] }) {
  const nodeName = useNodeName();
  return (
    <div className="vd-stagger space-y-6">
      <Card>
        <CardHeader title="Identity" subtitle="Engineering view" />
        <dl className="grid gap-x-6 gap-y-2 p-4 font-mono text-xs sm:grid-cols-2">
          <KV k="server_id" v={s.id} />
          <KV k="epoch" v={String(s.epoch)} />
          <KV k="service_id" v={s.service?.service_id} />
          <KV k="loopback_ip" v={s.service?.loopback_ip} />
          <KV
            k="ports"
            v={s.service?.ports?.map((p) => JSON.stringify(p)).join(" ")}
          />
          <KV k="deployment_id" v={s.deployment_id} />
          <KV
            k="desired / observed"
            v={`${s.desired_state} / ${s.observed_state}`}
          />
        </dl>
      </Card>
      <Card>
        <CardHeader
          title="Executions"
          subtitle="Each run holds a lease; a new epoch fences the previous one."
        />
        <div className="overflow-x-auto">
          <table className="w-full font-mono text-xs">
            <thead className="text-left uppercase tracking-wider text-muted">
              <tr>
                <th className="px-4 py-2">epoch</th>
                <th className="px-4 py-2">node</th>
                <th className="px-4 py-2">state</th>
                <th className="px-4 py-2">lease until</th>
                <th className="px-4 py-2">ended</th>
                <th className="px-4 py-2">id</th>
              </tr>
            </thead>
            <tbody className="divide-y divide-separator">
              {executions.map((e) => (
                <tr key={e.id}>
                  <td className="px-4 py-1.5">{e.epoch}</td>
                  <td className="px-4 py-1.5">{nodeName(e.node_id)}</td>
                  <td className="px-4 py-1.5">
                    {e.state}
                    {e.health ? ` (${e.health})` : ""}
                  </td>
                  <td className="px-4 py-1.5">
                    {e.ended_at ? "-" : dateTime(e.lease_expires_at)}
                  </td>
                  <td className="px-4 py-1.5">{e.end_reason ?? ""}</td>
                  <td className="px-4 py-1.5 text-muted">{e.id}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </Card>
    </div>
  );
}

function KV({ k, v }: { k: string; v?: string | null }) {
  return (
    <div className="flex gap-3">
      <dt className="w-28 shrink-0 text-muted sm:w-36">{k}</dt>
      <dd className="min-w-0 break-all text-foreground">{v ?? "-"}</dd>
    </div>
  );
}
