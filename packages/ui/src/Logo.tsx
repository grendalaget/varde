import { FAVICON, LOCKUP, MARK } from "./brandPaths";
import { cx } from "./ui";

/**
 * Brand v1 marks, same geometry and semantics as apps/web (stones use
 * currentColor; Glod lights only the host stone). "lit": the host is up.
 * "moving": the light hops between stones (failover/loading). "idle": mono.
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
      className={cx("vd w-auto text-foreground", className)}
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

/** The cairn alone. Uses the favicon art below 32 px, per the brand rules. */
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
