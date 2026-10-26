import { FAVICON } from "@varde/ui/brandPaths";
import { Mark, type MarkState } from "@varde/ui/Logo";
import { cx } from "@varde/ui/ui";

// The brand marks are shared with the tray and installer (packages/ui);
// what follows builds the dashboard's own status marks on top of them.
export { Logo, Mark, type MarkState } from "@varde/ui/Logo";

/** Failover loader: the lit stone hops between stones. Static when reduced motion is on. */
export function Loader({
  label = "Loading…",
  small = false,
  className,
  showLabel = !small,
}: {
  label?: string;
  small?: boolean;
  className?: string;
  showLabel?: boolean;
}) {
  return (
    <span
      role="status"
      className={cx(
        "inline-flex items-center gap-3 text-sm text-muted",
        className,
      )}
    >
      <Mark
        state="moving"
        small={small}
        className={
          small ? "h-3.5 w-3.5 text-current" : "h-10 w-10 text-foreground"
        }
      />
      {showLabel ? label : <span className="sr-only">{label}</span>}
    </span>
  );
}

/** Centered page-level loader. */
export function PageLoader({ label }: { label?: string }) {
  return (
    <div className="flex justify-center py-16">
      <Loader label={label} />
    </div>
  );
}

/** A single lit stone: the only on-screen use of Glød besides the logo. */
export function Ember({ className = "h-2.5 w-3" }: { className?: string }) {
  return (
    <svg
      viewBox="8.75 5.75 5.5 4.5"
      className={cx("vd shrink-0", className)}
      aria-hidden
    >
      <path className="vd-ember vd-ignite" d={FAVICON.host} />
    </svg>
  );
}

/** Marks the machine currently hosting a server. Ember is keyed by node so it re-ignites on Move or failover. */
export function HostMarker({
  nodeId,
  name,
  className,
}: {
  nodeId: string;
  name: string;
  className?: string;
}) {
  return (
    <span
      className={cx(
        "inline-flex items-center gap-1.5 text-foreground",
        className,
      )}
      title={`${name} is hosting this server`}
    >
      <Ember key={nodeId} />
      <span className="truncate">{name}</span>
    </span>
  );
}

const MOVING_STATES = new Set([
  "starting",
  "stopping",
  "recovering",
  "migrating",
]);

/** A server's own cairn: lit while hosted, hopping while the host changes. */
export function ServerMark({
  state,
  className = "h-8 w-8",
}: {
  state: string;
  className?: string;
}) {
  const s: MarkState =
    state === "running" ? "lit" : MOVING_STATES.has(state) ? "moving" : "idle";
  return (
    <Mark
      state={s}
      className={cx(s === "idle" ? "text-muted" : "text-foreground", className)}
    />
  );
}

/**
 * The cairn as a status mark inside a chip: lit while running, the ember hops
 * while the server changes hands, all stones cold when stopped, and the cap
 * stone tumbles off when failed.
 */
export function StateCairn({
  state,
  className = "h-3.5 w-3.5",
}: {
  state: string;
  className?: string;
}) {
  if (state === "failed") {
    return (
      <svg
        viewBox={FAVICON.viewBox}
        className={cx("vd shrink-0 text-foreground", className)}
        aria-hidden
      >
        <path className="vd-s" d={FAVICON.base} />
        <path className="vd-h vd-h2 vd-unlit" d={FAVICON.peer} />
        <path className="vd-h vd-h1 vd-unlit" d={FAVICON.host} />
        <path className="vd-h vd-h3 vd-fallen" d={FAVICON.cap} />
      </svg>
    );
  }
  const s: MarkState =
    state === "running" ? "lit" : MOVING_STATES.has(state) ? "moving" : "idle";
  return <Mark state={s} small className={cx("text-foreground", className)} />;
}
