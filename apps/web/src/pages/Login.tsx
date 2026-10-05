import { useState } from "react";
import { api, errorMessage } from "../api/client";
import Cairn from "../components/Cairn";
import { Button, ErrorNote, Field, inputClass } from "../components/ui";

export default function Login({ onAuthed }: { onAuthed: () => Promise<void> }) {
  const [mode, setMode] = useState<"login" | "signup">("login");
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [name, setName] = useState("");
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
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
    <main className="flex min-h-screen items-center justify-center bg-slate-950 p-6 text-slate-100">
      <div className="w-full max-w-sm">
        <div className="mb-8 flex flex-col items-center gap-3 text-center">
          <Cairn className="h-12 w-12" />
          <h1 className="text-2xl font-semibold">Varde</h1>
          <p className="text-sm text-slate-400">
            Game servers your group owns together — they keep running and keep
            your saves safe, whoever's PC is on.
          </p>
        </div>
        <form
          onSubmit={submit}
          className="space-y-4 rounded-lg border border-slate-800 bg-slate-900/60 p-6"
        >
          {mode === "signup" && (
            <Field label="Your name">
              <input
                className={inputClass}
                value={name}
                onChange={(e) => setName(e.target.value)}
                placeholder="Shown to your friends"
              />
            </Field>
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
          <p className="text-center text-sm text-slate-400">
            {mode === "login" ? "New here? " : "Already have an account? "}
            <button
              type="button"
              className="text-emerald-400 hover:underline"
              onClick={() => {
                setErr(null);
                setMode(mode === "login" ? "signup" : "login");
              }}
            >
              {mode === "login" ? "Create an account" : "Sign in"}
            </button>
          </p>
        </form>
      </div>
    </main>
  );
}
