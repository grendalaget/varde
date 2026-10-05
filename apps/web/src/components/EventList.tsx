import type { VardeEvent } from "../api/client";
import { describeEvent } from "../lib/activity";
import { dateTime, timeAgo } from "../lib/format";
import { Link } from "../lib/router";
import { useNodeName } from "../lib/session";
import { Empty } from "./ui";

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
    <ul className="divide-y divide-slate-800">
      {events.map((e) => (
        <li
          key={e.id}
          className="flex items-center justify-between gap-4 px-4 py-2 text-sm"
        >
          <span className="text-slate-200">
            {describeEvent(e, nodeName)}
            {showServer && e.server_id && (
              <>
                {" · "}
                <Link
                  to={`/servers/${e.server_id}`}
                  className="text-slate-400 hover:text-emerald-400"
                >
                  {serverNames?.[e.server_id] ?? "server"}
                </Link>
              </>
            )}
          </span>
          <span
            className="shrink-0 text-xs text-slate-500"
            title={dateTime(e.created_at)}
          >
            {timeAgo(e.created_at)}
          </span>
        </li>
      ))}
    </ul>
  );
}
