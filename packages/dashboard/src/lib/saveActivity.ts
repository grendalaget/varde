import type { VardeEvent } from "../api/client";

export type ActivityRow = {
  key: string;
  event: VardeEvent;
  steps?: VardeEvent[];
};

const pipeline = new Set([
  "snapshot.created",
  "snapshot.committed",
  "snapshot.replica_ready",
  "snapshot.superseded",
]);

export function groupSaveEvents(events: VardeEvent[]): ActivityRow[] {
  const rows: ActivityRow[] = [];
  const saves = new Map<string, ActivityRow>();
  const seen = new Set<number>();
  for (const event of [...events].sort((a, b) => b.id - a.id)) {
    if (seen.has(event.id)) continue;
    seen.add(event.id);
    const id = (event.data as Record<string, unknown> | undefined)?.snapshot_id;
    if (!pipeline.has(event.type) || typeof id !== "string" || !id) {
      rows.push({ key: `event:${event.id}`, event });
      continue;
    }
    const key = JSON.stringify([event.server_id, id]);
    let row = saves.get(key);
    if (!row) {
      row = { key: `save:${key}`, event, steps: [] };
      saves.set(key, row);
      rows.push(row);
    }
    row.steps!.unshift(event);
  }
  return rows;
}

export function describeSave(steps: VardeEvent[]): string {
  const count = new Set(
    steps
      .filter(
        (e) =>
          (e.type === "snapshot.replica_ready" ||
            e.type === "snapshot.created") &&
          e.node_id,
      )
      .map((e) => e.node_id),
  ).size;
  const safe = steps.some((e) => e.type === "snapshot.committed");
  const older = steps.some((e) => e.type === "snapshot.superseded");
  if (count && steps.some((e) => e.type === "snapshot.replica_ready"))
    return `Save backed up to ${count} ${count === 1 ? "machine" : "machines"}${older ? " · superseded" : safe ? "" : " · awaiting safety confirmation"}`;
  if (older) return "Older save superseded";
  if (safe) return "Save is safe";
  return "Save taken · waiting for backup";
}
