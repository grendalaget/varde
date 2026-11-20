import type { Server } from "../api/client";
import { timeAgo } from "../lib/format";
import { Badge, WarnNote } from "./ui";

type Holder = NonNullable<
  NonNullable<Server["summary"]["latest_safe_save"]>["replicas"]
>[number];

function HolderChips({
  replicas,
  compact,
  plain,
}: {
  replicas: Holder[];
  compact?: boolean;
  plain?: boolean;
}) {
  if (plain) {
    return (
      <div className="flex flex-col gap-1">
        {replicas.map((r) => (
          <span key={r.node_id} className={r.online ? "text-skifer-200" : "text-muted"}>
            {r.name}{r.anchor && " · always-on"}{!r.online && " · offline"}
          </span>
        ))}
      </div>
    );
  }
  return (
    <div className="flex flex-wrap items-center gap-1.5 text-xs">
      <span className="text-muted">Backed up on</span>
      {replicas.map((r) => (
        <Badge
          key={r.node_id}
          tone={r.anchor ? "violet" : "neutral"}
          title={
            !r.online && r.last_seen_at
              ? `Last seen ${timeAgo(r.last_seen_at)}`
              : undefined
          }
          className={r.online ? undefined : "opacity-50"}
        >
          {r.name}
          {r.anchor && !compact && " · always-on"}
          {!r.online && " · offline"}
        </Badge>
      ))}
    </div>
  );
}

/** "Backed up on" chips + offline/one-machine warnings. */
export default function SaveSafety({
  summary,
  compact,
  plain,
}: {
  summary: Server["summary"];
  compact?: boolean;
  plain?: boolean;
}) {
  const safe = summary.latest_safe_save;
  const newerUnsafe =
    summary.latest_save &&
    safe &&
    summary.latest_save.snapshot_id !== safe.snapshot_id;
  const replicas = safe?.replicas ?? summary.latest_save?.replicas ?? [];
  // Resume needs the newest save — evaluate its holders, not the older
  // committed save's, so a blocked server can't look resumable. No ready
  // holders at all is just as blocked as all-offline.
  const resumeReplicas =
    summary.latest_save?.replicas ?? safe?.replicas ?? [];
  const allOffline =
    summary.latest_save != null &&
    (resumeReplicas.length === 0 ||
      resumeReplicas.every((r) => !r.online));
  return (
    <div className="space-y-2">
      {replicas.length > 0 && (
        <HolderChips replicas={replicas} compact={compact} plain={plain} />
      )}
      {plain && replicas.length === 0 && <span className="text-muted">—</span>}
      {allOffline ? (
        <WarnNote>
          No machine with the latest save is online. Nothing is lost, but
          the newest save can't be restored until one comes back.
        </WarnNote>
      ) : (
        summary.only_on_one_machine && (
          <WarnNote>
            The latest save only exists on one machine. Keep it online until
            another machine (or an always-on backup) has a copy.
          </WarnNote>
        )
      )}
      {newerUnsafe && !compact && (
        <p className="text-xs text-muted">
          A newer save
          {summary.latest_save?.replicas?.length
            ? ` on ${summary.latest_save.replicas
                .map((r) => r.name)
                .join(", ")}`
            : ""}{" "}
          is still being backed up.
        </p>
      )}
    </div>
  );
}
