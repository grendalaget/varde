import { Button as HButton } from "@heroui/react";
import {
  useEffect,
  useRef,
  useState,
  type ButtonHTMLAttributes,
  type ReactNode,
} from "react";

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
  onClick,
  type,
  title,
}: {
  variant?: Variant;
  busy?: boolean;
  className?: string;
  children?: ReactNode;
  disabled?: boolean;
  onClick?: ButtonHTMLAttributes<HTMLButtonElement>["onClick"];
  type?: "button" | "submit" | "reset";
  title?: string;
}) {
  const variants = {
    primary: "primary",
    secondary: "secondary",
    danger: "danger-soft",
    ghost: "ghost",
  } as const;
  return (
    <HButton
      className={className}
      variant={variants[variant]}
      isDisabled={disabled || busy}
      isPending={busy}
      onPress={(e) => onClick?.(e as never)}
      type={type}
      {...(title ? ({ title } as Record<string, unknown>) : {})}
    >
      {children}
    </HButton>
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
}: {
  title: ReactNode;
  actions?: ReactNode;
  subtitle?: ReactNode;
}) {
  return (
    <div className="flex items-start justify-between gap-4 border-b border-separator px-4 py-3">
      <div>
        <h2 className="text-sm font-semibold text-foreground">{title}</h2>
        {subtitle && <p className="mt-0.5 text-xs text-muted">{subtitle}</p>}
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
}: {
  tone?: Tone;
  children: ReactNode;
  title?: string;
  className?: string;
}) {
  const t: Record<Tone, string> = {
    green: "bg-emerald-500/10 text-emerald-300 ring-emerald-500/30",
    warn: "bg-warn/10 text-warn ring-warn/30",
    red: "bg-rose-500/10 text-rose-300 ring-rose-500/30",
    blue: "bg-sky-500/10 text-sky-300 ring-sky-500/30",
    violet: "bg-violet-500/10 text-violet-300 ring-violet-500/30",
    neutral: "bg-default/60 text-muted ring-border",
    host: "bg-glod/10 text-foreground ring-glod/40",
  };
  return (
    <span
      title={title}
      className={cx(
        "inline-flex items-center gap-1 whitespace-nowrap rounded-full px-2 py-0.5 text-xs font-medium ring-1 ring-inset transition-colors duration-300 ease-vd-out",
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
    green: "bg-emerald-400",
    warn: "bg-warn",
    red: "bg-rose-400",
    blue: "bg-sky-400",
    violet: "bg-violet-400",
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
      <span className="text-sm font-medium text-foreground">{label}</span>
      <div className="mt-1">{children}</div>
      {help && <p className="mt-1 text-xs text-muted">{help}</p>}
    </label>
  );
}

export const inputClass =
  "w-full rounded-field border border-field-border bg-field px-3 py-1.5 text-sm text-field-foreground shadow-field outline-none transition-colors duration-150 ease-vd-out placeholder:text-field-placeholder hover:bg-field-hover focus:border-field-border-focus focus:bg-field-focus";

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
    <div className="vd-shake rounded-lg border border-danger/40 bg-danger-soft px-3 py-2 text-sm text-danger-soft-foreground">
      {children}
    </div>
  );
}

export function WarnNote({ children }: { children: ReactNode }) {
  return (
    <div className="vd-in rounded-lg border border-warning/40 bg-warning-soft px-3 py-2 text-sm text-warning-soft-foreground">
      {children}
    </div>
  );
}

export function Empty({ children }: { children: ReactNode }) {
  return (
    <div className="vd-fade px-4 py-10 text-center text-sm text-muted">
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
    <div className="vd-enter mb-6 flex flex-wrap items-end justify-between gap-4">
      <div>
        <h1 className="font-display text-2xl font-semibold tracking-tight text-foreground">
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
