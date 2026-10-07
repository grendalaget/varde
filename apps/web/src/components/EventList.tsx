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
    <ul className="vd-stagger divide-y divide-skifer-800">
      {events.map((e) => (
        <li
          key={e.id}
          className="flex items-center justify-between gap-4 px-4 py-2 text-sm"
        >
          <span className="min-w-0 text-skifer-200 [overflow-wrap:anywhere]">
            {describeEvent(e, nodeName)}
            {showServer && e.server_id && (
              <>
                {" · "}
                <Link
                  to={`/servers/${e.server_id}`}
                  className="text-skifer-400 hover:text-take"
                >
                  {serverNames?.[e.server_id] ?? "server"}
                </Link>
              </>
            )}
          </span>
          <span
            className="shrink-0 text-xs text-skifer-500"
            title={dateTime(e.created_at)}
          >
            {timeAgo(e.created_at)}
          </span>
        </li>
      ))}
    </ul>
  );
}
