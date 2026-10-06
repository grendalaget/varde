import { useMemo } from "react";
import { api, type VardeEvent } from "../api/client";
import { useLoad } from "./data";
import { useGroupEvents } from "./events";
import { useSession } from "./session";

export function describeEvent(
  e: VardeEvent,
  nodeName: (id?: string | null) => string,
): string {
  const d = (e.data ?? {}) as Record<string, unknown>;
  const node = nodeName(e.node_id);
  switch (e.type) {
    case "server.created":
    case "server.create":
      return "Server created";
    case "server.start":
    case "server.start_requested":
      return "Start requested";
    case "server.assigned":
    case "lease.issued":
      return `Hosting on ${node}`;
    case "server.stop":
    case "server.stop_requested":
      return "Stop requested";
    case "server.stopped":
      return `Stopped on ${node}`;
    case "server.failed":
      return `Failed on ${node}${d.message ? `: ${String(d.message)}` : ""}`;
    case "server.join_code":
      return `Join code is now ${String(d.join_code ?? "")}`;
    case "server.save_unavailable":
      return "Waiting for the newest save: it's only on offline machines";
    case "server.unschedulable":
      return `No machine can host it right now${d.reasons ? ` (${JSON.stringify(d.reasons)})` : ""}`;
    case "server.move":
    case "migration.started":
      return "Moving server";
    case "migration.completed":
      return `Moved to ${node}`;
    case "lease.expired":
      return `Lost contact with ${node}; recovering server`;
    case "execution.state":
      return `${node}: ${String(d.state ?? "")}`;
    case "snapshot.created":
      return `Save taken on ${node}`;
    case "snapshot.replica_ready":
      return `Save backed up on ${node}`;
    case "snapshot.committed":
      return "Save is safe";
    case "snapshot.superseded":
      return "Older save superseded";
    case "snapshot.request":
      return "Save requested";
    case "node.enrolled":
    case "node.enroll":
      return `${node} joined the group`;
    case "node.online":
      return `${node} is online`;
    case "node.suspect":
      return `${node} isn't responding`;
    case "node.offline":
      return `${node} went offline`;
    case "node.update":
      return `${node} settings changed`;
    case "node.delete":
      return `${String(d.name ?? node)} was removed`;
    default:
      return e.type;
  }
}

/** History from the API merged with live SSE events, newest first. */
export function useMergedEvents(filter?: (e: VardeEvent) => boolean) {
  const { group } = useSession();
  const { recent } = useGroupEvents();
  const hist = useLoad(
    () =>
      api.GET("/v1/groups/{groupId}/events", {
        params: { path: { groupId: group.group_id }, query: { limit: 200 } },
      }),
    [group.group_id],
  );
  return useMemo(() => {
    const byId = new Map<number, VardeEvent>();
    for (const e of hist.data?.events ?? []) byId.set(e.id, e);
    for (const e of recent) byId.set(e.id, e);
    let all = [...byId.values()].sort((a, b) => b.id - a.id);
    if (filter) all = all.filter(filter);
    return all;
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [hist.data, recent]);
}
