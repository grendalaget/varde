import { useEffect, useRef, useState, type ReactNode } from "react";
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
  WarnNote,
} from "../components/ui";
import { HostMarker, PageLoader, StateCairn } from "../components/Logo";
import { JoinCs2Button } from "../components/JoinCs2Button";
import { Icon } from "../components/icons";
import { ServerActions } from "../components/ServerActions";
import { useLoad } from "../lib/data";
import { useOnEvent } from "../lib/events";
import { bytes, dateTime, timeAgo } from "../lib/format";
import { Link, navigate } from "../lib/router";
import { canAdmin, useNodeName, useSession } from "../lib/session";
import EventList from "../components/EventList";
import { useMergedEvents } from "../lib/activity";
import { ConfigInput } from "../components/ConfigInput";
import { ControlSelect } from "../components/ControlSelect";
import { ControlHint, HintText } from "../components/ControlHint";
import { ControlNumber } from "../components/ControlNumber";
import { useConfirmation } from "../components/Confirmation";
import { isWideField } from "../lib/configField";
import SaveSafety from "../components/SaveSafety";
import { INVITE_TERMS, useInvite } from "../lib/invite";
import {
  activeExecution,
  crashed,
  isStartingUp,
  restartsOnSave,
  startupSteps,
} from "../lib/serverStatus";
import {
  askedFields,
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
  const { group, selectGroup } = useSession();
  const gid = server.data?.group_id;
  // a link can open a server from another group: show that group's machines
  useEffect(() => {
    if (gid && gid !== group.group_id) selectGroup(gid);
  }, [gid, group.group_id, selectGroup]);

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
    <div className="server-detail">
      <div className="mb-3 text-sm">
        <Link
          to="/"
          className="inline-flex items-center gap-1.5 text-muted transition-colors hover:text-foreground"
        >
          <Icon name="back" className="size-3.5" />
          Servers
        </Link>
      </div>
      <header className="vd-enter detail-heading">
        <div className="flex flex-wrap items-center justify-between gap-5">
          <div className="min-w-0">
            <div className="min-w-0">
              <div className="flex flex-wrap items-center gap-x-3 gap-y-1">
                <h1 className="vd-page-title">{s.name}</h1>
              </div>
              <p className="mt-0.5 text-sm text-muted">
                {games.name(s.game_id)}
              </p>
            </div>
          </div>
          <ServerActions
            key={s.id}
            s={s}
            primary={tab !== "settings"}
            failed={crashed(s, execs.data?.executions ?? [])}
            onChange={() => Promise.all([server.refresh(), execs.refresh()])}
          />
        </div>
      </header>
      <ServerFacts
        s={s}
        crossplay={isCrossplayEnabled(games.get(s.game_id), s.config)}
      />
      <div role="tablist" aria-label="Server sections" className="detail-tabs">
        {tabs.map(([t, label]) => (
          <button
            key={t}
            role="tab"
            aria-selected={tab === t}
            onClick={() => setTab(t)}
            className="transition-colors duration-300 ease-out"
          >
            {label}
          </button>
        ))}
      </div>
      <div key={tab} className="vd-in detail-panel">
        {tab === "overview" && (
          <Overview
            s={s}
            executions={execs.data?.executions ?? []}
            crossplay={isCrossplayEnabled(games.get(s.game_id), s.config)}
            onLogs={() => setTab("logs")}
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
    </div>
  );
}

function ServerFacts({ s, crossplay }: { s: Server; crossplay: boolean }) {
  const sum = s.summary;
  const address = s.public_url || (crossplay ? sum.join_code : sum.address);
  return (
    <dl className="hero-facts">
      <div>
        <dt>Status</dt>
        <dd className="flex items-center gap-2">
          <StateCairn state={s.observed_state} className="h-5 w-5" />
          {s.observed_state.replace(/^./, (c) => c.toUpperCase())}
        </dd>
      </div>
      <div className="address-fact">
        <dt>{crossplay && !s.public_url ? "Join code" : "Address"}</dt>
        <dd className="big-address">
          {address ? (
            <CopyText text={address} href={s.public_url || undefined} plain />
          ) : (
            <span className="text-muted">
              {crossplay && s.observed_state === "running"
                ? "Waiting for code…"
                : "—"}
            </span>
          )}
        </dd>
      </div>
      <div>
        <dt>Hosting on</dt>
        <dd className="fact-host">
          {sum.hosting_on ? (
            <HostMarker
              nodeId={sum.hosting_on.node_id}
              name={sum.hosting_on.name}
            />
          ) : (
            <span className="text-muted">Not running</span>
          )}
        </dd>
      </div>
      <div className="save-fact">
        <dt>Latest safe save</dt>
        <dd className="big-save">
          {sum.latest_safe_save ? (
            <time
              dateTime={new Date(sum.latest_safe_save.created_at).toISOString()}
            >
              {timeAgo(sum.latest_safe_save.created_at)}
            </time>
          ) : (
            "None yet"
          )}
        </dd>
        <dt className="replica-label">Backed up on</dt>
        <dd className="replica-line">
          <SaveSafety summary={sum} plain />
        </dd>
      </div>
    </dl>
  );
}

function Overview({
  s,
  executions,
  crossplay,
  onLogs,
}: {
  s: Server;
  executions: Execution[];
  crossplay: boolean;
  onLogs: () => void;
}) {
  const { group, nodes } = useSession();
  const nodeName = useNodeName();
  const sum = s.summary;
  const hostId = sum.hosting_on?.node_id;
  const otherHosts = nodes.filter(
    (n) =>
      n.hosting_enabled &&
      n.admin_state === "active" &&
      n.online &&
      n.id !== hostId,
  ).length;
  const lastRun = executions[0];
  const events = useMergedEvents(
    (e) => e.server_id === s.id && e.type !== "execution.state",
  );
  return (
    <div className="detail-overview">
      {isStartingUp(s) && (
        <StartupProgress
          s={s}
          exec={activeExecution(executions)}
          host={nodeName}
        />
      )}
      <section className="detail-connect">
        <h2 className="vd-section-title">Connect</h2>
        <Card>
          <div className="space-y-3 p-4 text-sm">
            {crashed(s, executions) && (
              <WarnNote>
                <p>
                  The game stopped unexpectedly
                  {lastRun ? ` on ${nodeName(lastRun.node_id)}` : ""}
                  {lastRun?.message ? `: ${lastRun.message}` : "."}
                </p>
                <p className="mt-1">
                  Varde won&apos;t restart it by itself. Check the logs, then
                  press Try again.{" "}
                  <button
                    type="button"
                    className="underline underline-offset-2"
                    onClick={onLogs}
                  >
                    Open logs
                  </button>
                </p>
              </WarnNote>
            )}
            {s.desired_state === "running" &&
              s.observed_state === "failed" &&
              !crashed(s, executions) && (
                <WarnNote>
                  No PC in the group can host this server right now. Varde keeps
                  trying and starts it when a PC that can run it is online with
                  hosting turned on.
                </WarnNote>
              )}
            <h3 className="font-medium">Join the game</h3>
            {!s.public_url && s.game_id === "cs2" && (
              <Cs2Join s={s} />
            )}
            {s.public_url ? (
              <p>Open the address above in your browser to join.</p>
            ) : crossplay ? (
              <p className="text-xs text-muted">
                In Valheim, choose Join game → Join by code; check here after a
                restart or move.
              </p>
            ) : (
              <p className="text-xs text-muted">
                Join from any group machine with Varde running; the address
                stays the same after a move.
              </p>
            )}
            <h3 className="border-t border-separator pt-4 font-medium">Host</h3>
            {sum.hosting_on && (
              <p className="text-xs text-muted">
                {otherHosts
                  ? "Another machine can take over if the host goes offline."
                  : "Keep the hosting machine online; no other machine can take over yet."}
              </p>
            )}
            {s.desired_state === "running" &&
              s.observed_state === "running" && (
                <ReadyCallout
                  groupId={group.group_id}
                  admin={canAdmin(group.role)}
                />
              )}
          </div>
        </Card>
      </section>
      <section className="detail-activity">
        <h2 className="vd-section-title">Recent activity</h2>
        <Card>
          <EventList events={events} limit={5} />
        </Card>
      </section>
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

function DetailSection({
  title,
  subtitle,
  actions,
  children,
}: {
  title: string;
  subtitle?: string;
  actions?: ReactNode;
  children: ReactNode;
}) {
  return (
    <section className="detail-section">
      <div className="mb-3 flex flex-wrap items-center justify-between gap-3">
        <div>
          <h2 className="vd-section-title">{title}</h2>
          {subtitle && <p className="mt-1 text-xs text-muted">{subtitle}</p>}
        </div>
        {actions}
      </div>
      <Card>{children}</Card>
    </section>
  );
}

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
    <DetailSection
      title="Saves"
      subtitle={`A save is safe once ${s.min_commit_replicas} ${s.min_commit_replicas === 1 ? "machine holds" : "machines hold"} it. Pinned saves are never cleaned up.`}
    >
      {snapshots.length === 0 ? (
        <Empty>No saves yet.</Empty>
      ) : (
        <div className="vd-scroll overflow-x-auto">
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
                      <HintText
                        className="text-foreground"
                        dateTime={new Date(x.created_at).toISOString()}
                        hint={dateTime(x.created_at)}
                      >
                        {timeAgo(x.created_at)}
                      </HintText>
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
                      <ControlHint text={x.pinned ? "Unpin" : "Keep this save forever"}>
                        <Button
                          variant="ghost"
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
                      </ControlHint>
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      )}
    </DetailSection>
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
    <DetailSection
      title="Logs"
      actions={
        executions.length > 0 && (
          <ControlSelect
            label="Execution"
            className="w-48 sm:w-64"
            value={effective}
            options={executions.map((e) => ({
              value: e.id,
              label: `${nodeName(e.node_id)} · ${e.started_at ? dateTime(e.started_at) : "not started"}${e.ended_at ? "" : " (current)"}`,
            }))}
            onChange={setExecId}
          />
        )
      }
    >
      <pre
        ref={box}
        className="vd-scroll max-h-[32rem] overflow-auto p-4 font-mono text-xs leading-relaxed text-foreground"
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
    </DetailSection>
  );
}

function ServerSettings({
  s,
  onSaved,
}: {
  s: Server;
  onSaved: () => Promise<void>;
}) {
  const confirmAction = useConfirmation();
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
  const restarts = restartsOnSave(s, withServerName(game, config, name));

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
            restarts
              ? "Saved. The server is restarting with the new settings."
              : "Saved",
          );
          await onSaved();
        }
      }}
    >
      <DetailSection title="Game">
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
      </DetailSection>
      <DetailSection title="Hosting & saves">
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
          <ControlNumber
            label="Save every (seconds)"
            min={30}
            value={interval}
            onChange={(value) => setIntervalS(Number(value))}
          />
          <ControlNumber
            label="Copies per save"
            min={1}
            value={rf}
            onChange={(value) => setRf(Number(value))}
          />
          <ControlNumber
            label="Copies before a save counts as safe"
            min={1}
            value={mcr}
            onChange={(value) => setMcr(Number(value))}
          />
        </div>
      </DetailSection>
      <ErrorNote>{err}</ErrorNote>
      <div className="flex items-center justify-between">
        {admin ? (
          <Button
            variant="danger"
            type="button"
            onClick={async () => {
              if (
                !(await confirmAction({
                  title: `Delete ${s.name} and its saves?`,
                  action: "Delete server",
                  expected: s.name,
                }))
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
        <div className="flex flex-wrap items-center justify-end gap-3">
          {restarts && !busy && (
            <span className="text-sm text-muted">
              Saving restarts the server now: players are disconnected and can
              rejoin.
            </span>
          )}
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
      <DetailSection title="Identity" subtitle="Engineering view">
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
      </DetailSection>
      <DetailSection
        title="Executions"
        subtitle="Each run holds a lease; a new epoch fences the previous one."
      >
        <div className="vd-scroll overflow-x-auto">
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
      </DetailSection>
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

function Cs2Join({ s }: { s: Server }) {
  const address = s.summary.address;
  if (!address)
    return (
      <p className="text-xs text-muted">
        Start the server to get the command for joining from CS2.
      </p>
    );
  return (
    <div className="space-y-2.5">
      <JoinCs2Button server={s} />
      <div className="flex flex-wrap items-center justify-between gap-2">
        <span className="font-medium text-foreground">
          Copy the CS2 join command
        </span>
        <CopyText text={`connect ${address}`} />
      </div>
      {s.observed_state !== "running" && (
        <p className="text-xs text-muted">
          The server isn&apos;t running yet. Start it first, or CS2 can&apos;t
          connect.
        </p>
      )}
      <p className="text-xs text-muted">
        With Varde running, enable CS2&apos;s developer console (~), then paste
        the join command.
      </p>
    </div>
  );
}

function ReadyCallout({ groupId, admin }: { groupId: string; admin: boolean }) {
  const inv = useInvite(groupId);
  return (
    <div>
      <p className="mb-2 text-xs text-muted">
        Invite friends to join the group.
      </p>
      {admin && (
        <Button busy={inv.busy} onClick={() => void inv.create()}>
          Invite friends
        </Button>
      )}
      {inv.link && (
        <Card className="vd-pop -mt-3 mb-6">
          <CardHeader title="Invite link" subtitle={INVITE_TERMS} />
          <div className="p-4">
            <CopyText text={inv.link} />
          </div>
        </Card>
      )}
      <ErrorNote>{inv.error}</ErrorNote>
    </div>
  );
}

function StartupProgress({
  s,
  exec,
  host,
}: {
  s: Server;
  exec: Execution | undefined;
  host: (id?: string | null) => string;
}) {
  const title =
    s.observed_state === "recovering"
      ? "Recovering the server"
      : s.observed_state === "migrating"
        ? "Moving the server"
        : "Starting the server";
  return (
    <Card>
      <CardHeader
        title={title}
        subtitle={
          exec
            ? `On ${host(exec.node_id)}. Varde shows each step as the PC reports it; there's no download percentage or time estimate.`
            : s.observed_state === "recovering"
              ? "The PC hosting it stopped answering. Varde is waiting for a PC that can host it."
              : "Waiting for a PC in the group to take it."
        }
      />
      <ol className="space-y-2 p-4 text-sm">
        {startupSteps(exec).map((step) => (
          <li
            key={step.key}
            aria-current={step.status === "current" ? "step" : undefined}
            className={cx(
              "flex items-center gap-2.5",
              step.status === "todo" ? "text-muted" : "text-foreground",
            )}
          >
            <Dot
              tone={
                step.status === "done"
                  ? "green"
                  : step.status === "current"
                    ? "blue"
                    : "neutral"
              }
            />
            {step.label}
            {step.status === "current" &&
              step.key === "preparing" &&
              s.game_id === "cs2" && (
                <span className="text-xs text-muted">
                  The first start on a PC without CS2 downloads the game server
                  through Steam, which can take a long time.
                </span>
              )}
          </li>
        ))}
      </ol>
    </Card>
  );
}
