import type { Server } from "../api/client";
import { Badge, WarnNote } from "./ui";

/** "Backed up on" chips + the one-machine warning. */
export default function SaveSafety({
  summary,
  compact,
}: {
  summary: Server["summary"];
  compact?: boolean;
}) {
  const safe = summary.latest_safe_save;
  const newerUnsafe =
    summary.latest_save &&
    safe &&
    summary.latest_save.snapshot_id !== safe.snapshot_id;
  const replicas = safe?.replicas ?? [];
  return (
    <div className="space-y-2">
      {replicas.length > 0 && (
        <div className="flex flex-wrap items-center gap-1.5 text-xs">
          <span className="text-skifer-400">Backed up on</span>
          {replicas.map((r) => (
            <Badge key={r.node_id} tone={r.anchor ? "violet" : "neutral"}>
              {r.name}
              {r.anchor && !compact && " · always-on"}
            </Badge>
          ))}
        </div>
      )}
      {summary.only_on_one_machine && (
        <WarnNote>
          The latest save only exists on one machine. Keep it online until
          another machine (or an always-on backup) has a copy.
        </WarnNote>
      )}
      {newerUnsafe && !compact && (
        <p className="text-xs text-skifer-400">
          A newer save is still being backed up.
        </p>
      )}
    </div>
  );
}
