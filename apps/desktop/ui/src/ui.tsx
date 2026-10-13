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
      className="vd-shake mt-3.5 w-fit max-w-full rounded-lg border border-danger/40 bg-danger-soft px-3.5 py-2.5 text-sm text-danger-soft-foreground"
    >
      {children}
    </div>
  );
}

// Last line of defence for error text: the real translation happens at
// the boundaries (cp_api::user_message, the IPC handlers), so a string
// that still looks like plumbing is swapped for a safe line rather than
// painting "api 404 http_error: 404 page not found" on the screen.
export function describeError(e: unknown): string {
  // `String(new Error("x"))` yields "Error: x" - strip the class name so an
  // Error-typed rejection with a friendly message still shows it, while one
  // holding plumbing ("Error: api 404 http_error: ...") is caught below.
  const s = String(e).replace(/^Error:\s*/, "");
  if (
    /http_error|page not found|transport error|error trying to connect|status:?\s*\d{3}|mock:/i.test(
      s,
    )
  ) {
    return "Something went wrong. Try again, or restart Varde if it keeps happening.";
  }
  return s;
}

export function WarnNote({ children }: { children: ReactNode }) {
  return (
    <div className="vd-in mt-3.5 w-fit max-w-full rounded-lg border border-warning/40 bg-warning-soft px-3.5 py-2.5 text-sm text-warning-soft-foreground">
      {children}
    </div>
  );
}
