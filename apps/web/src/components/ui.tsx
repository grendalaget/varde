import type { ButtonHTMLAttributes, ReactNode } from "react";

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
    primary: "bg-emerald-500 text-slate-950 hover:bg-emerald-400",
    secondary:
      "bg-slate-800 text-slate-100 hover:bg-slate-700 border border-slate-700",
    danger:
      "bg-rose-600/10 text-rose-300 hover:bg-rose-600/20 border border-rose-700/50",
    ghost: "text-slate-300 hover:bg-slate-800",
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
      {busy && (
        <span className="h-3 w-3 animate-spin rounded-full border-2 border-current border-t-transparent" />
      )}
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
        "rounded-lg border border-slate-800 bg-slate-900/60",
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
    <div className="flex items-start justify-between gap-4 border-b border-slate-800 px-4 py-3">
      <div>
        <h2 className="text-sm font-semibold text-slate-100">{title}</h2>
        {subtitle && (
          <p className="mt-0.5 text-xs text-slate-400">{subtitle}</p>
        )}
      </div>
      {actions && <div className="flex shrink-0 gap-2">{actions}</div>}
    </div>
  );
}

type Tone = "green" | "amber" | "red" | "blue" | "slate" | "violet";

export function Badge({
  tone = "slate",
  children,
  title,
}: {
  tone?: Tone;
  children: ReactNode;
  title?: string;
}) {
  const t: Record<Tone, string> = {
    green: "bg-emerald-500/10 text-emerald-300 ring-emerald-500/30",
    amber: "bg-amber-500/10 text-amber-300 ring-amber-500/30",
    red: "bg-rose-500/10 text-rose-300 ring-rose-500/30",
    blue: "bg-sky-500/10 text-sky-300 ring-sky-500/30",
    violet: "bg-violet-500/10 text-violet-300 ring-violet-500/30",
    slate: "bg-slate-500/10 text-slate-300 ring-slate-500/30",
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
    amber: "bg-amber-400",
    red: "bg-rose-400",
    blue: "bg-sky-400",
    violet: "bg-violet-400",
    slate: "bg-slate-500",
  };
  return (
    <span
      className={cx("inline-block h-2 w-2 shrink-0 rounded-full", t[tone])}
    />
  );
}

const SERVER_STATE: Record<string, { label: string; tone: Tone }> = {
  stopped: { label: "Stopped", tone: "slate" },
  starting: { label: "Starting", tone: "blue" },
  running: { label: "Running", tone: "green" },
  stopping: { label: "Stopping", tone: "amber" },
  recovering: { label: "Recovering server", tone: "amber" },
  migrating: { label: "Moving server", tone: "violet" },
  failed: { label: "Failed", tone: "red" },
};

export function ServerStateBadge({ state }: { state: string }) {
  const s = SERVER_STATE[state] ?? { label: state, tone: "slate" as Tone };
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
      <span className="text-sm font-medium text-slate-200">{label}</span>
      <div className="mt-1">{children}</div>
      {help && <p className="mt-1 text-xs text-slate-400">{help}</p>}
    </label>
  );
}

export const inputClass =
  "w-full rounded-md border border-slate-700 bg-slate-950 px-3 py-1.5 text-sm text-slate-100 placeholder:text-slate-500 focus:border-emerald-500 focus:outline-none focus:ring-1 focus:ring-emerald-500";

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
        checked ? "bg-emerald-500" : "bg-slate-700",
      )}
    >
      <span
        className={cx(
          "inline-block h-4 w-4 rounded-full bg-white shadow transition-transform",
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
    <div className="rounded-md border border-amber-800/60 bg-amber-950/30 px-3 py-2 text-sm text-amber-200">
      {children}
    </div>
  );
}

export function Empty({ children }: { children: ReactNode }) {
  return (
    <div className="px-4 py-10 text-center text-sm text-slate-400">
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
        <h1 className="text-xl font-semibold text-slate-50">{title}</h1>
        {subtitle && <p className="mt-1 text-sm text-slate-400">{subtitle}</p>}
      </div>
      {actions && <div className="flex gap-2">{actions}</div>}
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
        "rounded bg-slate-950 px-2 py-0.5 text-left text-sm text-emerald-300 ring-1 ring-slate-800 hover:ring-emerald-600",
        mono && "font-mono",
      )}
    >
      {text}
    </button>
  );
}
