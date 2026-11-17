import { expect, test } from "bun:test";
import type { Execution, Server } from "./src/api/client";
import {
  activeExecution,
  hostAwayNote,
  isStartingUp,
  restartsOnSave,
  startupSteps,
} from "./src/lib/serverStatus";

const exec = (over: Partial<Execution>): Execution =>
  ({
    id: "ex",
    server_id: "s",
    node_id: "n",
    epoch: 1,
    state: "preparing",
    action: "run",
    ...over,
  }) as Execution;

const server = (over: Partial<Server>): Server =>
  ({
    id: "s",
    desired_state: "running",
    observed_state: "starting",
    config: { game_mode: "competitive", map: "de_dust2" },
    ...over,
  }) as Server;

test("startup steps follow the agent's execution state", () => {
  expect(
    startupSteps(exec({ state: "starting" })).map((s) => s.status),
  ).toEqual(["done", "current", "todo"]);
  expect(startupSteps(undefined).every((s) => s.status === "todo")).toBe(true);
});

test("restoring is a step only when a save is restored", () => {
  const steps = startupSteps(
    exec({ state: "restoring", restore_snapshot_id: "snap" }),
  );
  expect(steps.map((s) => s.key)).toEqual([
    "preparing",
    "restoring",
    "starting",
    "running",
  ]);
  expect(steps[1].status).toBe("current");
});

test("active execution ignores ended and stopping runs", () => {
  expect(
    activeExecution([
      exec({ id: "stop", action: "stop" }),
      exec({ id: "old", ended_at: 1 }),
      exec({ id: "live" }),
    ])?.id,
  ).toBe("live");
});

test("starting up covers start, recovery and moves only while wanted running", () => {
  expect(isStartingUp(server({ observed_state: "recovering" }))).toBe(true);
  expect(isStartingUp(server({ observed_state: "running" }))).toBe(false);
  expect(isStartingUp(server({ desired_state: "stopped" }))).toBe(false);
});

test("only changed game settings on a running server restart it", () => {
  const s = server({ observed_state: "running" });
  expect(restartsOnSave(s, { map: "de_dust2", game_mode: "competitive" })).toBe(
    false,
  );
  expect(restartsOnSave(s, { game_mode: "retakes", map: "de_dust2" })).toBe(
    true,
  );
  expect(
    restartsOnSave(server({ desired_state: "stopped" }), { map: "de_inferno" }),
  ).toBe(false);
});

test("host-away note depends on whether another PC can take over", () => {
  expect(hostAwayNote("Ola-PC", 1)).toContain("restarts it on another");
  expect(hostAwayNote("Ola-PC", 0)).toContain("until Ola-PC is back");
});
