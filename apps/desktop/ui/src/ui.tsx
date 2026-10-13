// Shared UI helpers, kept in sync with apps/web/src/components/ui.tsx.
import { Button as HButton } from "@heroui/react";
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

export function ErrorNote({ children }: { children: ReactNode }) {
  if (!children) return null;
  return (
    <div
      role="alert"
      className="vd-shake rounded-lg border border-danger/40 bg-danger-soft px-3 py-2 text-sm text-danger-soft-foreground"
    >
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
