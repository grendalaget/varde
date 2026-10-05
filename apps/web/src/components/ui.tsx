import type { ButtonHTMLAttributes, ReactNode } from "react";
import { Loader } from "./Logo";

export function cx(...c: (string | false | null | undefined)[]) {
  return c.filter(Boolean).join(" ");
}

type Variant = "primary" | "secondary" | "danger" | "ghost";

export function Button({
  variant = "secondary",
  busy,
  className,
  children,
  disabled,
  ...rest
}: ButtonHTMLAttributes<HTMLButtonElement> & {
  variant?: Variant;
  busy?: boolean;
}) {
  const styles: Record<Variant, string> = {
    primary: "bg-take text-natt hover:bg-white",
    secondary:
      "bg-skifer-800 text-skifer-100 hover:bg-skifer-700 border border-skifer-700",
    danger:
      "bg-rose-600/10 text-rose-300 hover:bg-rose-600/20 border border-rose-700/50",
    ghost: "text-skifer-300 hover:bg-skifer-800",
  };
  return (
    <button
      className={cx(
        "inline-flex items-center justify-center gap-2 rounded-md px-3 py-1.5 text-sm font-medium transition-colors disabled:cursor-not-allowed disabled:opacity-50",
        styles[variant],
        className,
      )}
      disabled={disabled || busy}
      {...rest}
    >
      {busy && <Loader small label="Working…" />}
      {children}
    </button>
  );
}

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
        "rounded-lg border border-skifer-800 bg-skifer-900/60",
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
}: {
  title: ReactNode;
  actions?: ReactNode;
  subtitle?: ReactNode;
}) {
  return (
    <div className="flex items-start justify-between gap-4 border-b border-skifer-800 px-4 py-3">
      <div>
        <h2 className="text-sm font-semibold text-skifer-100">{title}</h2>
        {subtitle && (
          <p className="mt-0.5 text-xs text-skifer-400">{subtitle}</p>
        )}
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
}: {
  tone?: Tone;
  children: ReactNode;
  title?: string;
}) {
  const t: Record<Tone, string> = {
    green: "bg-emerald-500/10 text-emerald-300 ring-emerald-500/30",
    warn: "bg-warn/10 text-warn ring-warn/30",
    red: "bg-rose-500/10 text-rose-300 ring-rose-500/30",
    blue: "bg-sky-500/10 text-sky-300 ring-sky-500/30",
    violet: "bg-violet-500/10 text-violet-300 ring-violet-500/30",
    neutral: "bg-skifer-500/10 text-skifer-300 ring-skifer-500/30",
    host: "bg-glod/10 text-take ring-glod/40",
  };
  return (
    <span
      title={title}
      className={cx(
        "inline-flex items-center gap-1 whitespace-nowrap rounded-full px-2 py-0.5 text-xs font-medium ring-1 ring-inset",
        t[tone],
      )}
    >
      {children}
    </span>
  );
}

export function Dot({ tone }: { tone: Tone }) {
  const t: Record<Tone, string> = {
    green: "bg-emerald-400",
    warn: "bg-warn",
    red: "bg-rose-400",
    blue: "bg-sky-400",
    violet: "bg-violet-400",
    neutral: "bg-skifer-500",
    host: "bg-glod",
  };
  return (
    <span
      className={cx("inline-block h-2 w-2 shrink-0 rounded-full", t[tone])}
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
    <Badge tone={s.tone}>
      <Dot tone={s.tone} />
      {s.label}
    </Badge>
  );
}

export function Field({
  label,
  help,
  children,
}: {
  label: string;
  help?: ReactNode;
  children: ReactNode;
}) {
  return (
    <label className="block">
      <span className="text-sm font-medium text-skifer-200">{label}</span>
      <div className="mt-1">{children}</div>
      {help && <p className="mt-1 text-xs text-skifer-400">{help}</p>}
    </label>
  );
}

export const inputClass =
  "w-full rounded-md border border-skifer-700 bg-skifer-950 px-3 py-1.5 text-sm text-skifer-100 placeholder:text-skifer-500 focus:border-skifer-300 focus:outline-none focus:ring-1 focus:ring-skifer-300";

export function Toggle({
  checked,
  onChange,
  disabled,
  label,
}: {
  checked: boolean;
  onChange: (v: boolean) => void;
  disabled?: boolean;
  label?: string;
}) {
  return (
    <button
      type="button"
      role="switch"
      aria-checked={checked}
      aria-label={label}
      disabled={disabled}
      onClick={() => onChange(!checked)}
      className={cx(
        "relative inline-flex h-5 w-9 shrink-0 items-center rounded-full transition-colors disabled:opacity-50",
        checked ? "bg-take" : "bg-skifer-700",
      )}
    >
      <span
        className={cx(
          "inline-block h-4 w-4 rounded-full shadow transition-transform",
          checked ? "bg-natt" : "bg-take",
          checked ? "translate-x-4" : "translate-x-0.5",
        )}
      />
    </button>
  );
}

export function ErrorNote({ children }: { children: ReactNode }) {
  if (!children) return null;
  return (
    <div className="rounded-md border border-rose-800/60 bg-rose-950/40 px-3 py-2 text-sm text-rose-200">
      {children}
    </div>
  );
}

export function WarnNote({ children }: { children: ReactNode }) {
  return (
    <div className="rounded-md border border-warn/40 bg-warn/5 px-3 py-2 text-sm text-warn">
      {children}
    </div>
  );
}

export function Empty({ children }: { children: ReactNode }) {
  return (
    <div className="px-4 py-10 text-center text-sm text-skifer-400">
      {children}
    </div>
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
    <div className="mb-6 flex flex-wrap items-end justify-between gap-4">
      <div>
        <h1 className="text-2xl font-semibold tracking-tight text-skifer-50">
          {title}
        </h1>
        {subtitle && <p className="mt-1 text-sm text-skifer-400">{subtitle}</p>}
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
  return (
    <button
      type="button"
      title="Copy"
      onClick={() => void navigator.clipboard?.writeText(text)}
      className={cx(
        "rounded bg-skifer-950 px-2 py-0.5 text-left text-sm text-take ring-1 ring-skifer-700 hover:ring-skifer-400",
        mono && "font-mono",
      )}
    >
      {text}
    </button>
  );
}
