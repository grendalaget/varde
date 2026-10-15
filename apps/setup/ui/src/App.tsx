import { useEffect, useRef, useState } from "react";
import { Logo, Mark } from "./Logo";
import {
  invoke,
  listen,
  type ConfirmEvent,
  type ErrorEvent,
  type ProgressEvent,
  type UiState,
} from "./tauri";
import { Button, cx, ErrorNote, WarnNote } from "./ui";

type Screen = "intro" | "progress" | "confirm" | "done" | "error";

const INSTALL_LABELS: Record<string, string> = {
  webview2: "Checking WebView2",
  migrate: "Removing the previous Varde install",
  drain: "Stopping Varde",
  files: "Copying files",
  datadirs: "Preparing Varde's data folder",
  serverurl: "Saving server settings",
  registry: "Registering Varde",
  services: "Starting Varde",
  shortcuts: "Creating shortcuts",
  launch: "Launching Varde",
};
const UNINSTALL_LABELS: Record<string, string> = {
  drain: "Stopping Varde",
  registry: "Removing registry entries",
  files: "Removing Varde's files",
  data: "Removing app data",
};

function Option({
  checked,
  onChange,
  title,
  desc,
}: {
  checked: boolean;
  onChange: (v: boolean) => void;
  title: string;
  desc: string;
}) {
  return (
    <label className="group flex cursor-pointer items-start gap-3 rounded-card border border-border bg-surface-secondary px-3.5 py-3 transition-colors hover:border-accent/50">
      <input
        type="checkbox"
        className="peer sr-only"
        checked={checked}
        onChange={(e) => onChange(e.target.checked)}
      />
      <span
        aria-hidden
        className={cx(
          "mt-0.5 grid h-4.5 w-4.5 shrink-0 place-items-center rounded-[5px] border transition-colors",
          checked
            ? "border-accent bg-accent"
            : "border-field-border bg-field group-hover:border-accent/60",
        )}
      >
        <svg
          viewBox="0 0 12 12"
          className={cx(
            "h-3 w-3 text-accent-foreground transition-opacity",
            checked ? "opacity-100" : "opacity-0",
          )}
          fill="none"
          stroke="currentColor"
          strokeWidth="2.2"
          strokeLinecap="round"
          strokeLinejoin="round"
        >
          <path d="M2.2 6.3 4.8 8.9 9.8 3.1" />
        </svg>
      </span>
      <span className="min-w-0">
        <span className="block text-sm font-medium text-foreground">
          {title}
        </span>
        <span className="mt-0.5 block text-xs text-muted">{desc}</span>
      </span>
    </label>
  );
}

export default function App() {
  const [state, setState] = useState<UiState | null>(null);
  const [screen, setScreen] = useState<Screen>("intro");
  const [step, setStep] = useState<ProgressEvent | null>(null);
  const [confirmUrl, setConfirmUrl] = useState<string | null>(null);
  const [err, setErr] = useState<ErrorEvent | null>(null);
  const [autostart, setAutostart] = useState(true);
  const [cp, setCp] = useState(false);
  const [deleteData, setDeleteData] = useState(false);
  const started = useRef(false);

  useEffect(() => {
    void invoke<UiState>("get_state").then((s) => {
      setState(s);
      setAutostart(s.autostart);
      setCp(s.controlplane);
    });
    const un: Promise<() => void>[] = [
      listen<ProgressEvent>("setup-progress", (e) => {
        setStep(e.payload);
        setScreen((s) => (s === "intro" ? "progress" : s));
      }),
      listen<ConfirmEvent>("setup-confirm", (e) => {
        setConfirmUrl(e.payload.url);
        setScreen("confirm");
      }),
      listen<ErrorEvent>("setup-error", (e) => {
        setErr(e.payload);
        setScreen("error");
      }),
      listen("setup-done", () => setScreen("done")),
    ];
    return () => {
      un.forEach((p) => void p.then((f) => f()));
    };
  }, []);

  const isUninstall = state?.mode === "uninstall";
  const upgrading = !!state?.existing_version;
  const labels = isUninstall ? UNINSTALL_LABELS : INSTALL_LABELS;

  const begin = () => {
    if (started.current) return;
    started.current = true;
    setScreen("progress");
    if (isUninstall) {
      void invoke("start_uninstall", { deleteData });
    } else {
      void invoke("start_install", { autostart, controlplane: cp });
    }
  };

  const answer = (accept: boolean) => {
    void invoke("answer_http", { accept });
    setConfirmUrl(null);
    setScreen("progress");
  };

  return (
    <div className="flex h-screen w-full select-none flex-col overflow-hidden bg-background text-foreground">
      {/* title strip — frameless window, drag anywhere */}
      <div
        data-tauri-drag-region
        className="flex h-10 shrink-0 items-center justify-between pl-4 pr-2"
      >
        <Logo className="h-4 opacity-80" />
        <button
          onClick={() => void invoke("exit_app")}
          aria-label="Close"
          className="grid h-7 w-9 place-items-center rounded-md text-muted transition-colors hover:bg-surface-tertiary hover:text-foreground"
        >
          <svg viewBox="0 0 10 10" className="h-2.5 w-2.5">
            <path
              d="M1 1l8 8M9 1l-8 8"
              stroke="currentColor"
              strokeWidth="1.4"
              strokeLinecap="round"
            />
          </svg>
        </button>
      </div>

      <div className="flex min-h-0 flex-1 flex-col px-8 pb-7">
        {screen === "intro" && state && (
          <div className="vd-fade flex min-h-0 flex-1 flex-col">
            <div className="vd-pop mt-6">
              <Mark state="lit" className="h-14 w-14" />
            </div>
            <h1 className="vd-fade vd-d-1 mt-5 text-2xl font-semibold tracking-tight">
              {isUninstall
                ? "Remove Varde"
                : upgrading
                  ? "Update Varde"
                  : "Set up Varde"}
            </h1>
            <p className="vd-fade vd-d-2 mt-2 text-sm leading-relaxed text-muted">
              {isUninstall
                ? "This removes Varde from this PC, including the Varde service."
                : upgrading
                  ? `Varde ${state.existing_version} is installed. This updates it to ${state.version}; your data stays.`
                  : "One click and this PC joins your Varde."}
            </p>

            <div className="vd-fade vd-d-3 mt-5 flex flex-col gap-2">
              {!isUninstall && (
                <>
                  <Option
                    checked={autostart}
                    onChange={setAutostart}
                    title="Start Varde at login"
                    desc="Recommended for hosts"
                  />
                  <Option
                    checked={cp}
                    onChange={setCp}
                    title="Control plane on this PC"
                    desc="Run this group's Varde control plane here"
                  />
                </>
              )}
              {isUninstall && (
                <Option
                  checked={deleteData}
                  onChange={setDeleteData}
                  title="Also delete Varde's data"
                  desc="Saves and this machine's identity, in %ProgramData%\Varde"
                />
              )}
            </div>

            <div className="mt-auto pt-6">
              <Button
                variant={isUninstall ? "danger" : "primary"}
                className="w-full"
                onClick={begin}
              >
                {isUninstall
                  ? "Uninstall"
                  : upgrading
                    ? "Update"
                    : "Install"}
              </Button>
              <p className="mt-3 text-center text-xs text-muted">
                {isUninstall
                  ? "Varde's data stays on this PC unless you tick the box above."
                  : state.will_link
                    ? "Varde will open to link this PC when setup finishes."
                    : `Installs to ${state.instdir}`}
              </p>
            </div>
          </div>
        )}

        {screen === "progress" && (
          <div className="vd-fade flex min-h-0 flex-1 flex-col items-center justify-center">
            <Mark state="moving" className="h-14 w-14" />
            <h1 className="mt-5 text-xl font-semibold tracking-tight">
              {isUninstall ? "Removing Varde" : "Setting up Varde"}
            </h1>
            <p className="mt-1.5 text-sm text-muted">
              {step ? (labels[step.label] ?? step.label) : "Preparing"}
            </p>
            <div
              className="mt-7 h-1 w-64 overflow-hidden rounded-full bg-surface-tertiary"
              role="progressbar"
              aria-valuemin={0}
              aria-valuemax={step?.total ?? 100}
              aria-valuenow={step?.index ?? 0}
            >
              <div
                className="h-full rounded-full bg-accent transition-[width] duration-500 ease-vd-out"
                style={{
                  width: `${step ? (step.index / step.total) * 100 : 2}%`,
                }}
              />
            </div>
            <p className="mt-3 text-xs text-muted/70">
              {step ? `${step.index} of ${step.total}` : ""}
            </p>
          </div>
        )}

        {screen === "confirm" && (
          <div className="vd-fade flex min-h-0 flex-1 flex-col justify-center">
            <Mark state="idle" className="h-14 w-14" />
            <h1 className="mt-5 text-xl font-semibold tracking-tight">
              Keep this server address?
            </h1>
            <WarnNote>
              {confirmUrl} isn't encrypted (http://). Anyone on the network
              between this PC and Varde could read or change what it sends.
              Use it only for a Varde on your own network.
            </WarnNote>
            <div className="mt-5 flex gap-2">
              <Button variant="primary" onClick={() => answer(true)}>
                Keep it
              </Button>
              <Button variant="secondary" onClick={() => answer(false)}>
                Don't keep it
              </Button>
            </div>
          </div>
        )}

        {screen === "done" && (
          <div className="vd-fade flex min-h-0 flex-1 flex-col items-center justify-center text-center">
            <div className="vd-pop">
              <Mark state="lit" className="h-16 w-16" />
            </div>
            <h1 className="vd-fade vd-d-1 mt-5 text-2xl font-semibold tracking-tight">
              {isUninstall ? "Varde was removed" : "Varde is ready"}
            </h1>
            <p className="vd-fade vd-d-2 mt-2 max-w-72 text-sm leading-relaxed text-muted">
              {isUninstall
                ? "This PC no longer runs Varde."
                : state?.will_link
                  ? "Varde is opening to link this PC."
                  : "Varde is up to date and running."}
            </p>
            <Button
              variant="primary"
              className="vd-fade vd-d-3 mt-7 w-40"
              onClick={() => void invoke("exit_app")}
            >
              Done
            </Button>
          </div>
        )}

        {screen === "error" && (
          <div className="vd-fade flex min-h-0 flex-1 flex-col justify-center">
            <Mark state="idle" className="h-14 w-14" />
            <h1 className="mt-5 text-xl font-semibold tracking-tight">
              {isUninstall ? "Couldn't remove Varde" : "Couldn't set up Varde"}
            </h1>
            <ErrorNote detail={err?.detail}>
              {err?.message ?? "Something went wrong."}
            </ErrorNote>
            <div className="mt-5">
              <Button
                variant="secondary"
                className="w-40"
                onClick={() => void invoke("exit_app")}
              >
                Close
              </Button>
            </div>
          </div>
        )}
      </div>

      <div className="flex h-8 shrink-0 items-center justify-between px-4 text-[11px] text-muted/60">
        <span>Varde {state?.version ?? ""}</span>
        <span>varde.games</span>
      </div>
    </div>
  );
}
