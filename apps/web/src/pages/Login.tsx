import { useEffect, useState } from "react";
import { api, errorMessage } from "../api/client";
import { Logo } from "../components/Logo";
import { Button, ErrorNote, Field, inputClass } from "../components/ui";

export default function Login({
  onAuthed,
  federated,
  initialError,
}: {
  onAuthed: () => Promise<void>;
  federated?: boolean;
  initialError?: string | null;
}) {
  const [mode, setMode] = useState<"login" | "signup">("login");
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [name, setName] = useState("");
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(initialError ?? null);
  // initialError seeds mount state AND syncs later arrivals (a failed token
  // exchange on an already-open tab reports after Login is mounted).
  useEffect(() => {
    if (initialError) setErr(initialError);
  }, [initialError]);
  const invite = window.location.pathname.startsWith("/invite/")
    ? window.location.pathname.split("/")[2]
    : undefined;

  async function submit(e: React.FormEvent) {
    e.preventDefault();
    setBusy(true);
    setErr(null);
    const r =
      mode === "login"
        ? await api.POST("/v1/auth/login", { body: { email, password } })
        : await api.POST("/v1/auth/signup", {
            body: {
              email,
              password,
              display_name: name || email.split("@")[0],
              invite_code: invite,
            },
          });
    setBusy(false);
    if (r.error) setErr(errorMessage(r.error));
    else await onAuthed();
  }

  return (
    <main className="flex min-h-screen items-center justify-center bg-field p-6 text-foreground">
      <div className="w-full max-w-sm">
        <div className="vd-enter mb-8 flex flex-col items-center gap-3 text-center">
          <h1 className="vd-hover mb-2">
            <Logo className="h-16" />
          </h1>
          <p className="text-sm text-muted">
            Game servers your group owns together. They keep running and keep
            your saves safe, whoever's PC is on.
          </p>
        </div>
        <form
          onSubmit={submit}
          className="vd-enter vd-d-1 space-y-4 rounded-2xl border border-separator bg-surface p-6"
        >
          {mode === "signup" && (
            <div className="vd-reveal">
              <Field label="Your name">
                <input
                  className={inputClass}
                  value={name}
                  onChange={(e) => setName(e.target.value)}
                  placeholder="Shown to your friends"
                />
              </Field>
            </div>
          )}
          <Field label="Email">
            <input
              className={inputClass}
              type="email"
              autoComplete="email"
              required
              value={email}
              onChange={(e) => setEmail(e.target.value)}
            />
          </Field>
          <Field
            label="Password"
            help={mode === "signup" ? "At least 8 characters" : undefined}
          >
            <input
              className={inputClass}
              type="password"
              autoComplete={
                mode === "login" ? "current-password" : "new-password"
              }
              required
              minLength={mode === "signup" ? 8 : undefined}
              value={password}
              onChange={(e) => setPassword(e.target.value)}
            />
          </Field>
          <ErrorNote>{err}</ErrorNote>
          <Button
            variant="primary"
            className="w-full"
            busy={busy}
            type="submit"
          >
            {mode === "login" ? "Sign in" : "Create account"}
          </Button>
          <p className="text-center text-sm text-muted">
            {mode === "login" ? "New here? " : "Already have an account? "}
            <button
              type="button"
              className="text-foreground underline decoration-muted underline-offset-2 hover:decoration-foreground"
              onClick={() => {
                setErr(null);
                setMode(mode === "login" ? "signup" : "login");
              }}
            >
              {mode === "login" ? "Create an account" : "Sign in"}
            </button>
          </p>
        </form>
        {federated && (
          <div className="vd-enter vd-d-2 mt-4">
            <div className="mb-3 flex items-center gap-3 text-xs text-muted">
              <span className="h-px flex-1 bg-separator" />
              or
              <span className="h-px flex-1 bg-separator" />
            </div>
            <Button
              className="w-full"
              title="Use your varde.games account"
              onClick={() =>
                window.location.assign("https://varde.games/federate/cp?next=/")
              }
            >
              Continue with Varde
            </Button>
          </div>
        )}
      </div>
    </main>
  );
}
