// Shared UI helpers on Fluent UI (Microsoft's Fluent Design), keeping the
// same call shapes as before — kept in sync with apps/web where sensible.
import {
  Button as FButton,
  makeStyles,
  MessageBar,
  MessageBarBody,
  Spinner,
} from "@fluentui/react-components";
import type { ButtonHTMLAttributes, ReactNode } from "react";

export function cx(...c: (string | false | null | undefined)[]) {
  return c.filter(Boolean).join(" ");
}

type Variant = "primary" | "secondary" | "danger" | "ghost";

const useButtonStyles = makeStyles({
  danger: {
    color: "var(--danger)",
    ":hover": { color: "var(--danger)" },
    ":hover:active": { color: "var(--danger)" },
  },
});

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
  const styles = useButtonStyles();
  const appearances = {
    primary: "primary",
    secondary: "secondary",
    danger: "secondary",
    ghost: "subtle",
  } as const;
  return (
    <FButton
      appearance={appearances[variant]}
      className={cx(variant === "danger" && styles.danger, className)}
      disabled={disabled || busy}
      icon={busy ? <Spinner size="tiny" /> : undefined}
      onClick={(e) => onClick?.(e)}
      type={type}
      title={title}
    >
      {children}
    </FButton>
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
    <MessageBar intent="error" className="vd-shake mt-3.5 w-fit max-w-full">
      <MessageBarBody>
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
      </MessageBarBody>
    </MessageBar>
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
    <MessageBar intent="warning" className="vd-in mt-3.5 w-fit max-w-full">
      <MessageBarBody>{children}</MessageBarBody>
    </MessageBar>
  );
}
