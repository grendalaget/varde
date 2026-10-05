import { FAVICON, LOCKUP, MARK } from "./brandPaths";
import { cx } from "./ui";

/**
 * Brand v1 marks. Stones use currentColor (Tåke on Natt); Glød is only the lit
 * (host) stone. "lit": the host is up. "moving": the light hops between stones
 * (failover, loading). "idle": no stone is lit (mono).
 */
export type MarkState = "lit" | "moving" | "idle";

function stoneClass(state: MarkState, slot: 1 | 2 | 3) {
  return cx("vd-h", `vd-h${slot}`, state === "idle" && "vd-unlit");
}

/** Horizontal lockup (mark + wordmark), dark-background variant. */
export function Logo({
  className = "h-8",
  title = "Varde",
}: {
  className?: string;
  title?: string;
}) {
  return (
    <svg
      viewBox={LOCKUP.viewBox}
      className={cx("vd w-auto text-take", className)}
      role="img"
      aria-label={title}
    >
      <title>{title}</title>
      <path className="vd-s" d={LOCKUP.base} />
      <path className="vd-s" d={LOCKUP.slab} />
      <path className={stoneClass("lit", 2)} d={LOCKUP.peer} />
      <path className={stoneClass("lit", 1)} d={LOCKUP.host} />
      <path className={stoneClass("lit", 3)} d={LOCKUP.cap} />
      <path className="vd-s" d={LOCKUP.word} />
    </svg>
  );
}

/**
 * The cairn alone. Uses the favicon art when rendered below 32 px
 * (`small`), per the brand rules.
 */
export function Mark({
  state = "lit",
  small = false,
  className = "h-8 w-8",
  title,
}: {
  state?: MarkState;
  small?: boolean;
  className?: string;
  title?: string;
}) {
  const a11y = title
    ? { role: "img" as const, "aria-label": title }
    : { "aria-hidden": true as const };
  const p = small ? FAVICON : MARK;
  return (
    <svg
      viewBox={p.viewBox}
      className={cx(
        "vd shrink-0",
        state === "moving" && "vd-moving",
        className,
      )}
      {...a11y}
    >
      {title && <title>{title}</title>}
      <path className="vd-s" d={p.base} />
      {!small && <path className="vd-s" d={MARK.slab} />}
      <path className={stoneClass(state, 2)} d={p.peer} />
      <path className={stoneClass(state, 1)} d={p.host} />
      <path className={stoneClass(state, 3)} d={p.cap} />
    </svg>
  );
}

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
        "inline-flex items-center gap-3 text-sm text-skifer-400",
        className,
      )}
    >
      <Mark
        state="moving"
        small={small}
        className={small ? "h-3.5 w-3.5 text-current" : "h-10 w-10 text-take"}
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

/**
 * Marks the machine currently hosting a server. Keyed by node so the ember
 * re-ignites when the host changes (Move or failover).
 */
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
      key={nodeId}
      className={cx("inline-flex items-center gap-1.5 text-take", className)}
      title={`${name} is hosting this server`}
    >
      <Ember />
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
      className={cx(s === "idle" ? "text-skifer-600" : "text-take", className)}
    />
  );
}
