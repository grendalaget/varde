import type { Execution, Server } from "../api/client";

export type Step = {
  key: string;
  label: string;
  status: "done" | "current" | "todo";
};

const ORDER = ["preparing", "restoring", "starting", "running"] as const;

/** The agent's real execution phases, in order; restoring only when a save is restored. */
export function startupSteps(exec: Execution | undefined): Step[] {
  const keys = ORDER.filter(
    (k) => k !== "restoring" || !!exec?.restore_snapshot_id,
  );
  const at = exec ? keys.indexOf(exec.state as (typeof keys)[number]) : -1;
  const label: Record<string, string> = {
    preparing: "Getting game files ready",
    restoring: "Restoring the latest save",
    starting: "Starting the game",
    running: "Ready to join",
  };
  return keys.map((k, i) => ({
    key: k,
    label: label[k],
    status: at < 0 ? "todo" : i < at ? "done" : i === at ? "current" : "todo",
  }));
}

/** Active (not ended) execution for the server, newest first. */
export function activeExecution(
  executions: Execution[],
): Execution | undefined {
  return executions.find((e) => !e.ended_at && e.action === "run");
}

export function isStartingUp(s: Server): boolean {
  return (
    s.desired_state === "running" &&
    ["starting", "recovering", "migrating"].includes(s.observed_state)
  );
}

/** Saving changed game settings on a running server restarts it now. */
export function restartsOnSave(
  s: Server,
  config: Record<string, unknown>,
): boolean {
  return (
    s.desired_state === "running" &&
    JSON.stringify(sortKeys(config)) !==
      JSON.stringify(sortKeys(s.config ?? {}))
  );
}

function sortKeys(o: Record<string, unknown>) {
  return Object.fromEntries(
    Object.entries(o)
      .filter(([, v]) => v !== undefined)
      .sort(([a], [b]) => a.localeCompare(b)),
  );
}

/** What happens if the hosting PC sleeps, shuts down or loses its connection. */
export function hostAwayNote(host: string, otherHosts: number): string {
  return otherHosts > 0
    ? `Keep ${host} awake while you play. If it sleeps, shuts down or loses its connection, the game stops for everyone, then Varde restarts it on another online PC that can host, from the newest save an online PC has a copy of. Play since that save can be lost.`
    : `Keep ${host} awake while you play. If it sleeps, shuts down or loses its connection, the game stops for everyone until ${host} is back: no other online PC in the group can host it.`;
}
