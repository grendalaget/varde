import type { VardeEvent } from "../api/client";
import { describeEvent } from "../lib/activity";
import { dateTime, timeAgo } from "../lib/format";
import { Link } from "../lib/router";
import { useNodeName } from "../lib/session";
import { cx, Empty } from "./ui";

function dotTone(type: string) {
  if (/failed|unschedulable|expired/.test(type)) return "bg-danger";
  if (type.startsWith("snapshot.")) return "bg-info";
  if (
    /^(server\.assigned|lease\.issued|migration\.completed|node\.online|node\.enroll)/.test(
      type,
    )
  )
    return "bg-success";
  return "bg-skifer-500";
}

export default function EventList({
  events,
  showServer,
  serverNames,
}: {
  events: VardeEvent[];
  showServer?: boolean;
  serverNames?: Record<string, string>;
}) {
  const nodeName = useNodeName();
  if (events.length === 0) return <Empty>Nothing has happened yet.</Empty>;
  return (
    <ol className="vd-stagger px-4 py-2">
      {events.map((e, i) => (
        <li key={e.id} className="relative flex gap-3.5 py-2.5 text-sm">
          <span
            aria-hidden
            className="relative flex w-2 shrink-0 justify-center"
          >
            <span
              className={cx(
                "relative z-10 mt-1.5 size-2 rounded-full ring-4 ring-surface",
                dotTone(e.type),
              )}
            />
            {i < events.length - 1 && (
              <span className="absolute top-4 -bottom-4 w-px bg-separator" />
            )}
          </span>
          <span className="min-w-0 flex-1 text-foreground [overflow-wrap:anywhere]">
            {describeEvent(e, nodeName)}
            {showServer && e.server_id && (
              <>
                {" · "}
                <Link
                  to={`/servers/${e.server_id}`}
                  className="text-muted hover:text-foreground"
                >
                  {serverNames?.[e.server_id] ?? "server"}
                </Link>
              </>
            )}
          </span>
          <span
            className="mt-0.5 shrink-0 text-xs text-muted tabular-nums"
            title={dateTime(e.created_at)}
          >
            {timeAgo(e.created_at)}
          </span>
        </li>
      ))}
    </ol>
  );
}
