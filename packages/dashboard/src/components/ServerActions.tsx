import { useId, useRef, useState } from "react";
import { api, errorMessage, type Server } from "../api/client";
import { usePlatform } from "../lib/platform";
import { navigate } from "../lib/router";
import { canAdmin, useSession, useThisNode } from "../lib/session";
import { Button, cx, ErrorNote } from "./ui";

export function ServerActions({
  s,
  primary = false,
  compact = false,
  failed = s.observed_state === "failed",
  onChange,
}: {
  s: Server;
  primary?: boolean;
  compact?: boolean;
  failed?: boolean;
  onChange: () => Promise<unknown>;
}) {
  const { group, nodes, refreshNodes } = useSession();
  const thisNode = useThisNode();
  const { moveThisPc } = usePlatform();
  const [busy, setBusy] = useState<string | null>(null);
  const [err, setErr] = useState<{ msg: string; code?: string } | null>(null);
  const [moving, setMoving] = useState(false);
  const stopTitle = useId();
  const stopDialog = useRef<HTMLDialogElement>(null);
  const running = s.desired_state === "running";
  const hostId = s.summary.hosting_on?.node_id;
  const targets = nodes.filter(
    (n) =>
      n.hosting_enabled &&
      n.admin_state === "active" &&
      n.online &&
      n.id !== hostId,
  );
  async function run(name: string, fn: () => Promise<{ error?: unknown }>) {
    setBusy(name);
    setErr(null);
    try {
      const { error } = await fn();
      if (error) {
        const code = (error as { code?: string }).code;
        setErr({ msg: errorMessage(error), code });
      }
      await onChange();
    } catch (error) {
      setErr({ msg: errorMessage(error) });
    } finally {
      setBusy(null);
    }
  }

  const start = (allowOlder = false) =>
    run("start", () =>
      api.POST("/v1/servers/{serverId}/start", {
        params: { path: { serverId: s.id } },
        body: allowOlder ? { allow_older_snapshot: true } : {},
      }),
    );

  // A start no machine could take: offer the fix, this PC first.
  function startFix() {
    if (err?.code !== "unschedulable") return null;
    const here = nodes.find((n) => n.id === thisNode);
    if (here && !here.hosting_enabled) {
      return {
        msg: "This PC isn't allowed to host servers. Let it host and start the server here?",
        label: "Let this PC host",
        go: () =>
          run("fix", async () => {
            const r = await api.PATCH("/v1/nodes/{nodeId}", {
              params: { path: { nodeId: here.id } },
              body: { hosting_enabled: true },
            });
            if (r.error) return r;
            await refreshNodes();
            return api.POST("/v1/servers/{serverId}/start", {
              params: { path: { serverId: s.id } },
              body: {},
            });
          }),
      };
    }
    if (nodes.some((n) => n.hosting_enabled && n.admin_state === "active"))
      return null;
    // the approval page only offers groups the user administers
    if (thisNode && !here && canAdmin(group.role)) {
      return {
        msg: `This PC is linked to another group. Move it to ${group.name} to run the server here? It stops hosting for its current group.`,
        label: "Move this PC here",
        go: () => moveThisPc(group.group_id),
      };
    }
    return nodes.length === 0
      ? {
          msg: `${group.name} has no machines yet. Add this PC (or another machine) to run the server?`,
          label: "Add a machine",
          go: () => navigate("/machines/add"),
        }
      : {
          msg: `None of ${group.name}'s machines are allowed to host servers. Turn on hosting for one of them?`,
          label: "Open machines",
          go: () => navigate("/machines"),
        };
  }
  const fix = startFix();

  return (
    <div
      className={cx("relative flex flex-col gap-2", !compact && "items-end")}
    >
      <div className={cx("flex flex-wrap gap-2", !compact && "justify-end")}>
        {running ? (
          <>
            {failed && (
              <Button
                variant={primary ? "primary" : "secondary"}
                busy={busy === "start"}
                disabled={!!busy}
                onClick={() => void start()}
              >
                Try again
              </Button>
            )}
            {!compact && (
              <>
                <Button
                  variant={primary && !failed ? "primary" : "secondary"}
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
                  disabled={
                    s.observed_state !== "running" || targets.length === 0
                  }
                  onClick={() => setMoving(!moving)}
                >
                  Move…
                </Button>
              </>
            )}
            <Button
              variant="danger"
              busy={busy === "stop"}
              disabled={!!busy}
              onClick={() => stopDialog.current?.showModal()}
            >
              Stop
            </Button>
          </>
        ) : (
          <Button
            variant={primary ? "primary" : "secondary"}
            busy={busy === "start"}
            disabled={!!busy}
            onClick={() => void start()}
          >
            Start
          </Button>
        )}
      </div>
      <dialog
        ref={stopDialog}
        aria-labelledby={stopTitle}
        className="stop-dialog"
      >
        <h2 id={stopTitle} className="vd-section-title">
          Stop {s.name}?
        </h2>
        <p>
          Stop ends the game for everyone on it. Varde tries to take a final
          save first.
        </p>
        <div className="mt-5 flex justify-end gap-2">
          <Button onClick={() => stopDialog.current?.close()}>Cancel</Button>
          <Button
            variant="danger"
            onClick={() => {
              stopDialog.current?.close();
              void run("stop", () =>
                api.POST("/v1/servers/{serverId}/stop", {
                  params: { path: { serverId: s.id } },
                }),
              );
            }}
          >
            Stop server
          </Button>
        </div>
      </dialog>
      {moving && (
        <div className="vd-menu w-full max-w-sm space-y-2 rounded-md border border-border bg-surface p-3 text-sm shadow-lg">
          <p className="text-xs text-muted">
            {s.game_id === "minecraft"
              ? "Varde copies the save to the PC you pick and tries to hand players over without stopping. Players may still be disconnected; they can rejoin from this page."
              : "Varde stops the game, saves, copies the save to the PC you pick and starts it there. Players are disconnected and can rejoin from this page once it's running."}
          </p>
          <div className="flex flex-wrap items-center gap-2">
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
        </div>
      )}
      {err && (
        <div
          className="max-w-md space-y-2 text-left [overflow-wrap:anywhere]"
          role="alert"
        >
          <ErrorNote>
            {err.code === "latest_save_unavailable"
              ? "The latest save is on a machine that's offline right now. Wait for it to come back, or start from the newest save that's available (recent progress may be lost)."
              : err.code === "no_reachable_save"
                ? "No save is reachable right now. Wait for the machine holding it to come back online."
                : (fix?.msg ?? err.msg)}
          </ErrorNote>
          {fix && (
            <Button busy={busy === "fix"} onClick={() => void fix.go()}>
              {fix.label}
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
