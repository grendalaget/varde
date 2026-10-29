import { useRef, useState } from "react";
import { api, errorMessage, type Node } from "../api/client";
import type { paths } from "../api/schema";
import {
  Badge,
  Button,
  Callout,
  Card,
  cx,
  Dot,
  Empty,
  ErrorNote,
  PageHeader,
  Toggle,
} from "../components/ui";
import { Icon } from "../components/icons";
import { Ember } from "../components/Logo";
import { useLoad } from "../lib/data";
import { useOnEvent } from "../lib/events";
import { bytes, rtt, timeAgo } from "../lib/format";
import { navigate } from "../lib/router";
import { canAdmin, useSession } from "../lib/session";

type NodePatch = NonNullable<
  paths["/v1/nodes/{nodeId}"]["patch"]["requestBody"]
>["content"]["application/json"];

type Conn = { node_id: string; name?: string; path: string; rtt_us?: number };

export default function Machines() {
  const { nodes, group } = useSession();
  const hasAnchor = nodes.some((n) => n.anchor);
  const servers = useLoad(
    () =>
      api.GET("/v1/groups/{groupId}/servers", {
        params: { path: { groupId: group.group_id } },
      }),
    [group.group_id],
  );
  useOnEvent(
    (e) => !!e.server_id || e.type.startsWith("node."),
    servers.refresh,
  );
  const hosting = new Map<string, string[]>();
  for (const s of servers.data?.servers ?? []) {
    const id = s.summary.hosting_on?.node_id;
    if (id) hosting.set(id, [...(hosting.get(id) ?? []), s.name]);
  }
  return (
    <>
      <PageHeader
        title="Machines"
        subtitle="Every machine running Varde can host servers and keep backups."
        actions={
          canAdmin(group.role) && (
            <Button variant="primary" onClick={() => navigate("/machines/add")}>
              Add a machine
            </Button>
          )
        }
      />
      {nodes.length > 0 && !hasAnchor && (
        <Callout tone="violet" title="Tip: add an always-on backup">
          A NAS, home server or small VPS always has the latest save, even when
          everyone's PC is off.
        </Callout>
      )}
      {nodes.length === 0 ? (
        <Card>
          <Empty
            action={
              canAdmin(group.role) && (
                <Button
                  variant="primary"
                  onClick={() => navigate("/machines/add")}
                >
                  Add a machine
                </Button>
              )
            }
          >
            No machines yet. Add your PC to get started.
          </Empty>
        </Card>
      ) : (
        <div className="vd-stagger space-y-3">
          {nodes.map((n) => (
            <MachineRow key={n.id} n={n} hosting={hosting.get(n.id) ?? []} />
          ))}
        </div>
      )}
    </>
  );
}

function liveTone(n: Node) {
  const l = n.liveness ?? (n.online ? "online" : "offline");
  return l === "online" ? "green" : l === "suspect" ? "warn" : "neutral";
}

function MachineRow({ n, hosting }: { n: Node; hosting: string[] }) {
  const { group, refreshNodes } = useSession();
  const admin = canAdmin(group.role);
  const [err, setErr] = useState<string | null>(null);
  const [open, setOpen] = useState(false);
  const conns = (
    (n as Node & { connections?: Conn[] }).connections ?? []
  ).filter((c) => c.path !== "none");

  const [opt, setOpt] = useState<Partial<NodePatch>>({});
  const queue = useRef(Promise.resolve());
  // In-flight request count per patched key: an optimistic value stays until
  // every request that wrote that key settles, so true/false/true can't flip
  // the switch back mid-flight.
  const pending = useRef(new Map<keyof NodePatch, number>());

  // Optimistic: flip the switch now, reconcile after PATCH + refresh.
  // Patches serialize per row so rapid toggles can't settle out of order.
  function patch(body: NodePatch) {
    setErr(null);
    const keys = Object.keys(body) as (keyof NodePatch)[];
    for (const k of keys) {
      pending.current.set(k, (pending.current.get(k) ?? 0) + 1);
    }
    setOpt((o) => ({ ...o, ...body }));
    // catch() first: a rejected request must not wedge the queue
    queue.current = queue.current
      .catch(() => {})
      .then(async () => {
        const { error } = await api.PATCH("/v1/nodes/{nodeId}", {
          params: { path: { nodeId: n.id } },
          body,
        });
        if (error) {
          setErr(errorMessage(error));
        } else {
          // a refresh failure must not strand the optimistic value — the next
          // refresh reconciles the switch with the server anyway
          await refreshNodes().catch(() => {});
        }
        setOpt((o) => {
          const next = { ...o };
          for (const k of keys) {
            const left = (pending.current.get(k) ?? 1) - 1;
            if (left > 0) {
              pending.current.set(k, left);
            } else {
              pending.current.delete(k);
              delete next[k];
            }
          }
          return next;
        });
      });
  }

  const hostingEnabled = opt.hosting_enabled ?? n.hosting_enabled;
  const anchor = opt.anchor ?? n.anchor;

  const caps = n.capabilities;
  const live = n.liveness ?? (n.online ? "online" : "offline");
  return (
    <Card>
      <div className="flex flex-wrap items-center gap-4 px-4 py-3">
        <div className="flex min-w-0 basis-full items-center gap-3.5 sm:basis-0 sm:flex-1">
          <span className="relative shrink-0">
            <span
              className={cx(
                "grid size-10 place-items-center rounded-xl ring-1 ring-inset transition-colors duration-300",
                hosting.length > 0
                  ? "bg-glod/15 text-glod ring-glod/30"
                  : "bg-default text-muted ring-border",
              )}
            >
              <Icon
                name={n.os === "android" ? "phone" : "machine"}
                className="size-[22px]"
              />
            </span>
            <span className="absolute -right-0.5 -bottom-0.5 flex rounded-full ring-[2.5px] ring-surface">
              <Dot tone={liveTone(n)} />
            </span>
          </span>
          <div className="min-w-0">
            <div className="flex flex-wrap items-center gap-2">
              <span className="truncate font-medium text-foreground">
                {n.name}
              </span>
              {hosting.length > 0 && (
                <Badge
                  tone="host"
                  className="min-w-0 max-w-full"
                  title={`Hosting ${hosting.join(", ")}`}
                >
                  <Ember />
                  <span className="truncate">Hosting {hosting.join(", ")}</span>
                </Badge>
              )}
              {anchor && <Badge tone="violet">Always-on backup</Badge>}
              {n.admin_state !== "active" && (
                <Badge tone="warn">{n.admin_state}</Badge>
              )}
              {n.update_available && (
                <Badge
                  tone="warn"
                  title={`Agent ${n.agent_version ?? "?"} → ${n.update_available} is available`}
                >
                  Update {n.update_available}
                </Badge>
              )}
            </div>
            <div className="text-xs text-muted">
              {live === "online"
                ? "Online"
                : `Last seen ${timeAgo(n.last_seen_at)}`}
              {" · "}
              {n.os}/{n.arch}
              {caps?.memory_total_mb
                ? ` · ${Math.round(caps.memory_total_mb / 1024)} GB RAM`
                : ""}
              {caps?.disk_free_bytes
                ? ` · ${bytes(caps.disk_free_bytes)} free`
                : ""}
            </div>
          </div>
        </div>
        {conns.length > 0 && (
          <div className="flex flex-wrap gap-1.5">
            {conns.map((c) => (
              <Badge
                key={c.node_id}
                tone={c.path === "direct" ? "green" : "warn"}
                title={`Connection to ${c.name ?? c.node_id}`}
              >
                {c.name ?? c.node_id}:{" "}
                {c.path === "direct" ? "Direct" : "Relayed"}
                {c.rtt_us ? ` · ${rtt(c.rtt_us)}` : ""}
              </Badge>
            ))}
          </div>
        )}
        <Toggle
          checked={hostingEnabled}
          disabled={!admin}
          onChange={(v) => void patch({ hosting_enabled: v })}
          label="Can host"
        >
          <span className="text-sm text-foreground">Can host</span>
        </Toggle>
        <Button variant="ghost" onClick={() => setOpen(!open)}>
          {open ? "Less" : "More"}
        </Button>
      </div>
      {open && (
        <div className="vd-reveal space-y-4 border-t border-separator px-4 py-4 text-sm">
          <div className="flex items-start gap-3">
            <Toggle
              checked={anchor}
              disabled={!admin}
              onChange={(v) => void patch({ anchor: v })}
              label="Always-on backup"
            >
              <span>
                <span className="block text-foreground">Always-on backup</span>
                <span className="block text-xs text-muted">
                  Gets a copy of every save from every server. Use this for
                  machines that are always on.
                </span>
              </span>
            </Toggle>
          </div>
          <div className="grid gap-2 text-xs text-muted sm:grid-cols-2">
            <div>Agent version: {n.agent_version ?? "-"}</div>
            <div>
              Uptime:{" "}
              {caps?.uptime_s ? `${Math.round(caps.uptime_s / 3600)} h` : "-"}
            </div>
            <div>Runtimes: {caps?.runtimes?.join(", ") || "-"}</div>
            <div>On battery: {caps?.on_battery ? "yes" : "no"}</div>
            <div className="sm:col-span-2 truncate font-mono">ID {n.id}</div>
          </div>
          {admin && (
            <div className="flex flex-wrap gap-2">
              {n.admin_state === "active" ? (
                <Button onClick={() => void patch({ admin_state: "draining" })}>
                  Stop hosting here (drain)
                </Button>
              ) : (
                <Button onClick={() => void patch({ admin_state: "active" })}>
                  Re-enable
                </Button>
              )}
              <Button
                variant="danger"
                onClick={async () => {
                  if (
                    !confirm(
                      `Remove ${n.name} from the group? Its backups will no longer count.`,
                    )
                  )
                    return;
                  const { error } = await api.DELETE("/v1/nodes/{nodeId}", {
                    params: { path: { nodeId: n.id } },
                  });
                  if (error) setErr(errorMessage(error));
                  await refreshNodes();
                }}
              >
                Remove machine
              </Button>
            </div>
          )}
        </div>
      )}
      <ErrorNote>{err}</ErrorNote>
    </Card>
  );
}
