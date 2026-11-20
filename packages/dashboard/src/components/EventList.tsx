import { useId, useState } from "react";
import type { VardeEvent } from "../api/client";
import { describeEvent } from "../lib/activity";
import {
  describeSave,
  groupSaveEvents,
  type ActivityRow,
} from "../lib/saveActivity";
import { dateTime, timeAgo } from "../lib/format";
import { Link } from "../lib/router";
import { useNodeName } from "../lib/session";
import { Empty } from "./ui";

function EventRow({
  row,
  showServer,
  serverNames,
}: {
  row: ActivityRow;
  showServer?: boolean;
  serverNames?: Record<string, string>;
}) {
  const nodeName = useNodeName();
  const [open, setOpen] = useState(false);
  const id = useId();
  const e = row.event;
  const taken = row.steps?.find((step) => step.type === "snapshot.created");
  const when = taken?.created_at ?? e.created_at;
  const holders = [
    ...new Set(
      row.steps
        ?.filter(
          (step) =>
            step.node_id &&
            (step.type === "snapshot.created" ||
              step.type === "snapshot.replica_ready"),
        )
        .map((step) => nodeName(step.node_id)) ?? [],
    ),
  ];
  return (
    <li className="activity-row">
      <div className="activity-summary">
        <span
          aria-hidden
          className={`activity-dot ${/failed|expired|unschedulable/.test(e.type) ? "bg-danger" : "bg-skifer-500"}`}
        />
        <div className="min-w-0 flex-1">
          {row.steps ? (
            <button
              type="button"
              aria-expanded={open}
              aria-controls={id}
              onClick={() => setOpen(!open)}
              className="activity-disclosure"
            >
              {describeSave(row.steps)}{" "}
            </button>
          ) : (
            describeEvent(e, nodeName)
          )}
          {row.steps && (
            <span className="mt-1 block text-[10px] text-muted">
              {taken?.data?.reason
                ? `${String(taken.data.reason).replace(/^./, (c) => c.toUpperCase())} save`
                : "Save pipeline"}
              {holders.length ? ` · ${holders.join(" · ")}` : ""}
            </span>
          )}
          {showServer && e.server_id && (
            <Link
              to={`/servers/${e.server_id}`}
              className="mt-1 block text-xs text-muted hover:text-foreground"
            >
              {serverNames?.[e.server_id] ?? "server"}
            </Link>
          )}
        </div>
        <div className="activity-trailing">
          <time
            className="shrink-0 text-[10px] text-muted"
            dateTime={new Date(when).toISOString()}
            title={dateTime(when)}
          >
            {timeAgo(when)}
          </time>
          {row.steps && (
            <span
              aria-hidden
              className={open ? "activity-chevron is-open" : "activity-chevron"}
            >
              ›
            </span>
          )}
        </div>
      </div>
      {row.steps && (
        <div id={id} className="activity-steps" data-open={open} inert={!open}>
          <div>
            <ol>
              {row.steps.map((step) => (
                <li key={step.id}>
                  <span>{describeEvent(step, nodeName)}</span>
                  <time title={dateTime(step.created_at)}>
                    {dateTime(step.created_at)}
                  </time>
                </li>
              ))}
            </ol>
          </div>
        </div>
      )}
    </li>
  );
}

export default function EventList({
  events,
  showServer,
  serverNames,
  limit,
}: {
  events: VardeEvent[];
  showServer?: boolean;
  serverNames?: Record<string, string>;
  limit?: number;
}) {
  if (events.length === 0) return <Empty>Nothing has happened yet.</Empty>;
  return (
    <ol className="activity-list">
      {groupSaveEvents(events)
        .slice(0, limit)
        .map((row) => (
          <EventRow
            key={row.key}
            row={row}
            showServer={showServer}
            serverNames={serverNames}
          />
        ))}
    </ol>
  );
}
