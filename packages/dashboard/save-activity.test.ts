import { expect, test } from "bun:test";
import type { VardeEvent } from "./src/api/client";
import { describeSave, groupSaveEvents } from "./src/lib/saveActivity";

const event = (
  id: number,
  type: string,
  snapshot = "a",
  server = "s",
  node = "pc",
): VardeEvent => ({
  id,
  group_id: "group",
  type,
  server_id: server,
  node_id: node,
  data: { snapshot_id: snapshot },
  created_at: id,
});

test("complete pipelines group once with chronological steps and distinct backup machines", () => {
  const events = [
    event(1, "snapshot.created"),
    event(2, "snapshot.committed"),
    event(3, "snapshot.replica_ready"),
    event(4, "snapshot.replica_ready", "a", "s", "mini"),
    event(5, "snapshot.superseded"),
  ];
  const rows = groupSaveEvents([...events, events[3]]);
  expect(rows).toHaveLength(1);
  expect(rows[0].steps?.map((e) => e.id)).toEqual([1, 2, 3, 4, 5]);
  expect(describeSave(rows[0].steps!)).toBe(
    "Save backed up to 2 machines · superseded",
  );
});
test("incomplete pipelines never invent safety or backup steps", () => {
  const steps = [event(1, "snapshot.created")];
  expect(groupSaveEvents(steps)[0].steps).toEqual(steps);
  expect(describeSave(steps)).toBe("Save taken · waiting for backup");
  expect(describeSave([event(2, "snapshot.committed")])).toBe("Save is safe");
  expect(describeSave([event(3, "snapshot.replica_ready")])).toContain(
    "awaiting safety confirmation",
  );
});
test("interleaved saves and server lifecycle events stay independent and newest first", () => {
  const rows = groupSaveEvents([
    event(1, "snapshot.created"),
    event(2, "snapshot.created", "b"),
    event(3, "server.start"),
    event(4, "snapshot.committed"),
    event(5, "snapshot.replica_ready", "b"),
    event(6, "lease.expired"),
  ]);
  expect(rows.map((r) => r.event.id)).toEqual([6, 5, 4, 3]);
  expect(rows[1].steps?.map((e) => e.id)).toEqual([2, 5]);
  expect(rows[2].steps?.map((e) => e.id)).toEqual([1, 4]);
});
test("empty and malformed payloads remain safe, and server IDs scope save IDs", () => {
  expect(groupSaveEvents([])).toEqual([]);
  const bad = { ...event(1, "snapshot.created"), data: {} };
  expect(groupSaveEvents([bad])[0].steps).toBeUndefined();
  expect(
    groupSaveEvents([
      event(2, "snapshot.created"),
      event(3, "snapshot.created", "a", "other"),
    ]),
  ).toHaveLength(2);
  expect(
    groupSaveEvents([event(4, "snapshot.request")])[0].steps,
  ).toBeUndefined();
});
