// Shared UI helpers for the tray (apps/desktop/ui), installer (apps/setup/ui)
// and dashboard. apps/web/src/components/ui.tsx re-exports cx, Button, Field
// and inputClass from here and adds the dashboard's own, larger set.
import { Button as HButton } from "@heroui/react";
import { useId } from "react";
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
  group = false,
}: {
  label: string;
  help?: ReactNode;
  children: ReactNode;
  group?: boolean;
}) {
  const id = useId();
  const content = (
    <>
      <span
        id={group ? id : undefined}
        className="text-sm font-medium text-foreground"
      >
        {label}
      </span>
      <div className="mt-1">{children}</div>
      {help && <p className="mt-1 text-xs text-muted">{help}</p>}
    </>
  );
  return group ? (
    <div role="group" aria-labelledby={id} className="block">
      {content}
    </div>
  ) : (
    <label className="block">{content}</label>
  );
}

export const inputClass =
  "w-full rounded-field border border-field-border bg-field px-3 py-1.5 text-sm text-field-foreground shadow-field outline-none transition-colors duration-150 ease-vd-out placeholder:text-field-placeholder hover:bg-field-hover focus:border-field-border-focus focus:bg-field-focus";

export function ErrorNote({
  children,
  detail,
}: {
  children: ReactNode;
  // Technical form of the error, behind a disclosure for support/debugging.
  detail?: string;
}) {
  if (!children) return null;
  return (
    <div
      role="alert"
      className="vd-shake mt-3.5 w-fit max-w-full rounded-lg border border-danger/40 bg-danger-soft px-3.5 py-2.5 text-sm text-danger-soft-foreground"
    >
      {children}
      {detail ? (
        <details className="mt-1.5">
          <summary className="cursor-pointer text-xs opacity-80 underline-offset-2 transition-opacity hover:underline hover:opacity-100">
            Additional information
          </summary>
          <p className="mt-1.5 font-mono text-xs break-all whitespace-pre-wrap opacity-70">
            {detail}
          </p>
        </details>
      ) : null}
    </div>
  );
}

// Last line of defence for error text: the real translation happens at
// the boundaries (cp_api::user_message, the IPC handlers), so a string
// that still looks like plumbing is swapped for a safe line rather than
// painting "api 404 http_error: 404 page not found" on the screen.
const GENERIC_ERROR =
  "Something went wrong. Try again, or restart Varde if it keeps happening.";

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
    return GENERIC_ERROR;
  }
  return s;
}

// What the ErrorNote paints: `text` always user-facing, `detail` the
// technical original for the disclosure - either carried by the boundary
// (UiErr {message, detail}) or the raw value when it looked like plumbing.
export function errorParts(e: unknown): { text: string; detail?: string } {
  if (e && typeof e === "object" && !(e instanceof Error)) {
    const o = e as { message?: unknown; detail?: unknown };
    if (typeof o.message === "string") {
      return {
        text: describeError(o.message),
        detail: typeof o.detail === "string" && o.detail ? o.detail : undefined,
      };
    }
  }
  const text = describeError(e);
  return text === GENERIC_ERROR ? { text, detail: String(e) } : { text };
}

export function WarnNote({ children }: { children: ReactNode }) {
  return (
    <div className="vd-in mt-3.5 w-fit max-w-full rounded-lg border border-warning/40 bg-warning-soft px-3.5 py-2.5 text-sm text-warning-soft-foreground">
      {children}
    </div>
  );
}
