import { cx } from "@varde/ui/ui";
import { Mark, StateCairn } from "./Logo";
import { useEffect, useRef, useState, type ReactNode } from "react";

// Shared with the tray and installer (packages/ui); the rest of this file is
// the dashboard's own set.
export { Button, cx, Field, inputClass } from "@varde/ui/ui";

export function Card({
  children,
  className,
}: {
  children: ReactNode;
  className?: string;
}) {
  return (
    <div
      className={cx(
        "rounded-2xl border border-separator bg-surface shadow-surface",
        className,
      )}
    >
      {children}
    </div>
  );
}

export function CardHeader({
  title,
  actions,
  subtitle,
  icon,
}: {
  title: ReactNode;
  actions?: ReactNode;
  subtitle?: ReactNode;
  icon?: ReactNode;
}) {
  return (
    <div className="flex items-start justify-between gap-4 border-b border-separator px-4 py-3">
      <div className="flex min-w-0 items-center gap-3">
        {icon && (
          <span className="grid size-9 shrink-0 place-items-center rounded-xl bg-default text-muted ring-1 ring-inset ring-border">
            {icon}
          </span>
        )}
        <div className="min-w-0">
          <h2 className="text-sm font-semibold text-foreground">{title}</h2>
          {subtitle && <p className="mt-0.5 text-xs text-muted">{subtitle}</p>}
        </div>
      </div>
      {actions && <div className="flex shrink-0 gap-2">{actions}</div>}
    </div>
  );
}

/** `host` (Glød) marks the machine currently hosting a server, and nothing else. */
type Tone = "green" | "warn" | "red" | "blue" | "neutral" | "violet" | "host";

export function Badge({
  tone = "neutral",
  children,
  title,
  className,
  slab = false,
}: {
  tone?: Tone;
  children: ReactNode;
  title?: string;
  className?: string;
  /** Stone-slab outline instead of a generic pill. */
  slab?: boolean;
}) {
  const t: Record<Tone, string> = {
    green: "bg-success/10 text-success ring-success/30",
    warn: "bg-warning/10 text-warning ring-warning/30",
    red: "bg-danger/10 text-danger ring-danger/30",
    blue: "bg-info/10 text-info ring-info/30",
    violet: "bg-violet/10 text-violet ring-violet/30",
    neutral: "bg-default/60 text-muted ring-border",
    host: "bg-glod/10 text-foreground ring-glod/40",
  };
  return (
    <span
      title={title}
      className={cx(
        "inline-flex items-center gap-1 whitespace-nowrap px-2 py-0.5 text-xs font-medium ring-1 ring-inset transition-colors duration-300 ease-vd-out",
        slab ? "rounded-[9px_12px_8px_11px]" : "rounded-full",
        t[tone],
        className,
      )}
    >
      {children}
    </span>
  );
}

export function Dot({ tone }: { tone: Tone }) {
  const t: Record<Tone, string> = {
    green: "bg-success",
    warn: "bg-warning",
    red: "bg-danger",
    blue: "bg-info",
    violet: "bg-violet",
    neutral: "bg-muted",
    host: "bg-glod",
  };
  return (
    <span
      className={cx(
        "inline-block h-2 w-2 shrink-0 rounded-full transition-colors duration-300",
        t[tone],
      )}
    />
  );
}

const SERVER_STATE: Record<string, { label: string; tone: Tone }> = {
  stopped: { label: "Stopped", tone: "neutral" },
  starting: { label: "Starting", tone: "blue" },
  running: { label: "Running", tone: "green" },
  stopping: { label: "Stopping", tone: "warn" },
  recovering: { label: "Recovering server", tone: "warn" },
  migrating: { label: "Moving server", tone: "violet" },
  failed: { label: "Failed", tone: "red" },
};

export function ServerStateBadge({ state }: { state: string }) {
  const s = SERVER_STATE[state] ?? { label: state, tone: "neutral" as Tone };
  return (
    <Badge tone={s.tone} slab>
      <StateCairn state={state} />
      {s.label}
    </Badge>
  );
}

export function Toggle({
  checked,
  onChange,
  disabled,
  label,
  children,
}: {
  checked: boolean;
  onChange: (v: boolean) => void;
  disabled?: boolean;
  label?: string;
  children?: ReactNode;
}) {
  return (
    <button
      type="button"
      role="switch"
      aria-checked={checked}
      aria-label={label}
      disabled={disabled}
      onClick={() => onChange(!checked)}
      className="inline-flex items-center gap-2 text-left disabled:opacity-50"
    >
      <span
        className={cx(
          "relative inline-flex h-5 w-9 shrink-0 items-center rounded-full transition-colors duration-200 ease-vd-out",
          checked ? "bg-accent" : "bg-field-border",
        )}
      >
        <span
          className={cx(
            "inline-block h-4 w-4 rounded-full shadow transition-transform duration-300 ease-vd-spring active:scale-90",
            checked ? "bg-accent-foreground" : "bg-foreground",
            checked ? "translate-x-4" : "translate-x-0.5",
          )}
        />
      </span>
      {children}
    </button>
  );
}

export function ErrorNote({ children }: { children: ReactNode }) {
  if (!children) return null;
  return (
    <div className="vd-shake w-fit max-w-full rounded-lg border border-danger/40 bg-danger-soft px-3.5 py-2.5 text-sm text-danger-soft-foreground">
      {children}
    </div>
  );
}

export function WarnNote({ children }: { children: ReactNode }) {
  return (
    <div className="vd-in w-fit max-w-full rounded-lg border border-warning/40 bg-warning-soft px-3.5 py-2.5 text-sm text-warning-soft-foreground">
      {children}
    </div>
  );
}

export function Empty({
  children,
  action,
}: {
  children: ReactNode;
  action?: ReactNode;
}) {
  return (
    <div className="vd-fade flex flex-col items-center gap-4 px-6 py-12 text-center">
      <Mark state="idle" className="h-10 w-10 text-skifer-600" />
      <div className="max-w-sm text-sm text-muted">{children}</div>
      {action}
    </div>
  );
}

/** Highlighted note with optional actions, e.g. "this group has no machines". */
export function Callout({
  title,
  children,
  actions,
  tone = "accent",
}: {
  title: ReactNode;
  children?: ReactNode;
  actions?: ReactNode;
  tone?: "accent" | "violet";
}) {
  return (
    <div
      role="status"
      className={cx(
        "vd-enter mb-6 flex flex-wrap items-center gap-4 rounded-2xl border px-5 py-4",
        tone === "accent"
          ? "border-accent/30 bg-accent-soft"
          : "border-violet/30 bg-violet/10",
      )}
    >
      <div className="min-w-0 flex-1">
        <h2 className="font-semibold text-foreground">{title}</h2>
        {children && <p className="mt-0.5 text-sm text-muted">{children}</p>}
      </div>
      {actions && <div className="flex flex-wrap gap-2">{actions}</div>}
    </div>
  );
}

/** Initials on a colour picked from the name, for people and groups. */
export function Avatar({
  name,
  className = "size-8 rounded-full text-xs",
}: {
  name: string;
  className?: string;
}) {
  let hue = 0;
  for (const c of name) hue = (hue * 31 + c.charCodeAt(0)) % 360;
  const initials = name
    .split(/\s+/)
    .filter(Boolean)
    .slice(0, 2)
    .map((w) => w[0].toUpperCase())
    .join("");
  return (
    <span
      aria-hidden
      className={cx(
        "grid shrink-0 place-items-center font-semibold text-natt shadow-[inset_0_1px_0_rgb(255_255_255/0.3)]",
        className,
      )}
      style={{
        background: `linear-gradient(145deg, oklch(80% 0.09 ${hue}), oklch(62% 0.11 ${hue}))`,
      }}
    >
      {initials || "?"}
    </span>
  );
}

/** A radio option drawn as a card; group several under role="radiogroup". */
export function ChoiceCard({
  selected,
  onSelect,
  title,
  sub,
  disabled,
}: {
  selected: boolean;
  onSelect: () => void;
  title: string;
  sub?: ReactNode;
  disabled?: boolean;
}) {
  return (
    <button
      type="button"
      role="radio"
      aria-checked={selected}
      disabled={disabled}
      onClick={onSelect}
      className={cx(
        "flex items-center gap-3 rounded-xl border px-3.5 py-3 text-left transition-colors duration-200 ease-vd-out focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-accent disabled:opacity-50",
        selected
          ? "border-accent/70 bg-accent-soft"
          : "border-separator bg-surface hover:border-field-border",
      )}
    >
      <span
        aria-hidden
        className={cx(
          "grid size-4 shrink-0 place-items-center rounded-full border-2 transition-colors",
          selected ? "border-accent" : "border-field-border",
        )}
      >
        <span
          className={cx(
            "size-1.5 rounded-full bg-accent transition-transform duration-200 ease-vd-spring",
            selected ? "scale-100" : "scale-0",
          )}
        />
      </span>
      <span className="min-w-0">
        <span className="block truncate text-sm font-medium text-foreground">
          {title}
        </span>
        {sub && (
          <span className="flex items-center gap-1.5 text-xs text-muted">
            {sub}
          </span>
        )}
      </span>
    </button>
  );
}

export function PageHeader({
  title,
  subtitle,
  actions,
}: {
  title: ReactNode;
  subtitle?: ReactNode;
  actions?: ReactNode;
}) {
  return (
    <div className="vd-enter mb-6 flex flex-wrap items-end justify-between gap-4">
      <div>
        <h1 className="font-display text-[1.75rem] leading-tight font-semibold tracking-tight text-foreground">
          {title}
        </h1>
        {subtitle && <p className="mt-1 text-sm text-muted">{subtitle}</p>}
      </div>
      {actions && <div className="flex flex-wrap gap-2">{actions}</div>}
    </div>
  );
}

export function CopyText({
  text,
  mono = true,
}: {
  text: string;
  mono?: boolean;
}) {
  const [copied, setCopied] = useState(false);
  const timer = useRef<ReturnType<typeof setTimeout>>(undefined);
  useEffect(() => {
    setCopied(false);
    return () => clearTimeout(timer.current);
  }, [text]);
  return (
    <button
      type="button"
      title={copied ? "Copied" : "Copy"}
      onClick={() => {
        if (!navigator.clipboard) return;
        void navigator.clipboard.writeText(text).then(
          () => {
            setCopied(true);
            clearTimeout(timer.current);
            timer.current = setTimeout(() => setCopied(false), 1400);
          },
          () => {},
        );
      }}
      className={cx(
        "group inline-flex items-center gap-1.5 rounded bg-field px-2 py-0.5 text-left text-sm ring-1 transition-all duration-200 ease-vd-out active:scale-[0.96]",
        copied
          ? "text-emerald-300 ring-emerald-500/50"
          : "text-foreground ring-field-border hover:ring-field-border-focus",
        mono && "font-mono",
      )}
    >
      {text}
      <span className="relative -mr-0.5 h-3.5 w-3.5 shrink-0">
        <svg
          viewBox="0 0 24 24"
          fill="none"
          stroke="currentColor"
          strokeWidth="2"
          strokeLinecap="round"
          strokeLinejoin="round"
          className={cx(
            "absolute inset-0 transition-all duration-200 ease-vd-out",
            copied
              ? "scale-50 opacity-0"
              : "scale-100 opacity-40 group-hover:opacity-100",
          )}
        >
          <rect width="14" height="14" x="8" y="8" rx="2" ry="2" />
          <path d="M4 16c-1.1 0-2-.9-2-2V4c0-1.1.9-2 2-2h10c1.1 0 2 .9 2 2" />
        </svg>
        <svg
          viewBox="0 0 24 24"
          fill="none"
          stroke="currentColor"
          strokeWidth="2.5"
          strokeLinecap="round"
          strokeLinejoin="round"
          className={cx(
            "absolute inset-0 text-emerald-400 transition-all duration-200 ease-vd-spring",
            copied ? "scale-100 opacity-100" : "scale-50 opacity-0",
          )}
        >
          <path d="M20 6 9 17l-5-5" />
        </svg>
      </span>
    </button>
  );
}

/** Warm Glød wash across the top of a page; parent must be relative. */
export function TopGlow() {
  return (
    <div
      aria-hidden
      className="pointer-events-none absolute inset-x-0 top-0 h-96 bg-[radial-gradient(60%_100%_at_50%_0%,color-mix(in_oklab,var(--accent)_7%,transparent),transparent)]"
    />
  );
}
