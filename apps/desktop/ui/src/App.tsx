// Link / status window. Talks only to the tray process (typed commands),
// which talks to the local Varde service.

import {
  Input,
  Label,
  ListBox,
  Select,
  Spinner,
  TextField,
} from "@heroui/react";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Logo, Mark } from "./Logo";
import {
  invoke,
  listen,
  type FedState,
  type LinkDefaults,
  type UiStatus,
} from "./tauri";
import {
  Button,
  describeError,
  ErrorNote,
  errorParts,
  Field,
  WarnNote,
} from "./ui";

type ErrParts = { text: string; detail?: string };

const LINKED = ["connecting", "online", "offline"];

const hostOf = (u: string) => u.replace(/^[a-z]+:\/\//i, "").split("/")[0];

// the hosted control plane: only it can verify varde.games sign-in tokens
const HOSTED_CP_HOST = "cp.varde.games";
const HOSTED_CP_URL = "https://cp.varde.games";

type IconProps = { className?: string };

const iconClass = "h-4 w-4 shrink-0";

function CopyIcon({ className = iconClass }: IconProps) {
  return (
    <svg
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth="2"
      strokeLinecap="round"
      strokeLinejoin="round"
      className={className}
      aria-hidden
    >
      <rect width="14" height="14" x="8" y="8" rx="2" ry="2" />
      <path d="M4 16c-1.1 0-2-.9-2-2V4c0-1.1.9-2 2-2h10c1.1 0 2 .9 2 2" />
    </svg>
  );
}

function OpenIcon({ className = iconClass }: IconProps) {
  return (
    <svg
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth="2"
      strokeLinecap="round"
      strokeLinejoin="round"
      className={className}
      aria-hidden
    >
      <path d="M15 3h6v6" />
      <path d="M10 14 21 3" />
      <path d="M18 13v6a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V8a2 2 0 0 1 2-2h6" />
    </svg>
  );
}

function CloseIcon({ className = iconClass }: IconProps) {
  return (
    <svg
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth="2"
      strokeLinecap="round"
      strokeLinejoin="round"
      className={className}
      aria-hidden
    >
      <path d="M18 6 6 18" />
      <path d="m6 6 12 12" />
    </svg>
  );
}

function DashboardIcon({ className = iconClass }: IconProps) {
  return (
    <svg
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth="2"
      strokeLinecap="round"
      strokeLinejoin="round"
      className={className}
      aria-hidden
    >
      <rect width="7" height="9" x="3" y="3" rx="1" />
      <rect width="7" height="5" x="14" y="3" rx="1" />
      <rect width="7" height="9" x="14" y="12" rx="1" />
      <rect width="7" height="5" x="3" y="16" rx="1" />
    </svg>
  );
}

function PowerIcon({ className = iconClass }: IconProps) {
  return (
    <svg
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth="2"
      strokeLinecap="round"
      strokeLinejoin="round"
      className={className}
      aria-hidden
    >
      <path d="M12 2v10" />
      <path d="M18.4 6.6a9 9 0 1 1-12.8 0" />
    </svg>
  );
}

type View = "wait" | "form" | "fed" | "code" | "done" | "linked" | "noagent";

function describe(s: UiStatus) {
  if (s.group_name && s.node_name)
    return `Linked to ${s.group_name} as ${s.node_name}.`;
  if (s.group_name) return `Linked to ${s.group_name}.`;
  return "Linked. Connecting to Varde...";
}

export default function App() {
  const [defaults, setDefaults] = useState<LinkDefaults | null>(null);
  const [status, setStatus] = useState<UiStatus | null>(null);
  const [seen, setSeen] = useState(false);
  const [graceDone, setGraceDone] = useState(false);
  const [started, setStarted] = useState(false);
  const [autoPending, setAutoPending] = useState(false);
  const [forceForm, setForceForm] = useState(false);
  const [url, setUrl] = useState("");
  const [urlEdited, setUrlEdited] = useState(false);
  const [serverOpen, setServerOpen] = useState(false);
  // read by stable callbacks; state mirror above is for render
  const urlEditedRef = useRef(false);
  const [code, setCode] = useState("");
  const [now, setNow] = useState(() => Date.now());
  const [formError, setFormError] = useState<ErrParts | null>(null);
  const [codeError, setCodeError] = useState<ErrParts | null>(null);
  const [linkedError, setLinkedError] = useState<ErrParts | null>(null);
  const [noagentError, setNoagentError] = useState<ErrParts | null>(null);
  const [busy, setBusy] = useState<Record<string, boolean>>({});
  const [linkBusy, setLinkBusy] = useState(false);
  const [copied, setCopied] = useState(false);
  // "Log in with Varde" progress; null until the button is clicked
  const [fed, setFed] = useState<FedState | null>(null);
  const [fedGroup, setFedGroup] = useState("");
  const closeTimer = useRef<number | undefined>(undefined);

  // stable (reads the ref): a re-subscribing effect could otherwise race
  // cleanup and leave a stale link-defaults listener that treats the
  // address as unedited and overwrites the user's edit
  const applyDefaults = useCallback((d: LinkDefaults) => {
    setDefaults(d);
    if (!urlEditedRef.current) setUrl(d.url);
    if (d.auto) setAutoPending(true);
  }, []);

  // boot: defaults, status stream, current status
  useEffect(() => {
    let offStatus: (() => void) | undefined;
    let offDefaults: (() => void) | undefined;
    let offFed: (() => void) | undefined;
    (async () => {
      const d = await invoke<LinkDefaults>("link_defaults").catch(() => null);
      if (d) applyDefaults(d);
      offStatus = await listen<UiStatus | null>("status", (e) =>
        setStatus(e.payload),
      );
      // the tray was started again with --link while this window was open
      offDefaults = await listen<LinkDefaults>("link-defaults", (e) =>
        applyDefaults(e.payload),
      );
      offFed = await listen<FedState>("federated-auth", (e) => {
        const p = e.payload;
        setFed(p);
        if (p.stage === "pick_group") setFedGroup(p.groups[0]?.id ?? "");
      });
      // a deep link may have progressed the sign-in before this window
      // subscribed - hydrate the current stage
      const f = await invoke<FedState | null>("federated_state").catch(
        () => null,
      );
      if (f) {
        setFed(f);
        if (f.stage === "pick_group") setFedGroup(f.groups[0]?.id ?? "");
      }
      setStatus(await invoke<UiStatus | null>("get_status").catch(() => null));
    })();
    return () => {
      offStatus?.();
      offDefaults?.();
      offFed?.();
    };
  }, [applyDefaults]);

  // the tray connects within moments of starting; wait briefly before
  // calling the service dead
  useEffect(() => {
    const t = setTimeout(() => setGraceDone(true), 3000);
    return () => clearTimeout(t);
  }, []);

  useEffect(() => {
    const t = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(t);
  }, []);

  const formState =
    !!status &&
    (status.state === "not_linked" ||
      (!!defaults?.relink && LINKED.includes(status.state)));

  // side effects that follow a new status
  useEffect(() => {
    const s = status;
    if (!s) return;
    setSeen(true);
    setForceForm(false);
    // a federated sign-in is done once ITS link flow reaches the service -
    // during a re-link the old link still reports a LINKED state until the
    // new one is approved, so wait for our own link to have started
    if (started && LINKED.includes(s.state)) setFed(null);
    if (s.state === "linking" && s.user_code) {
      setStarted(true);
      // a new code: drop a refusal shown for the previous one
      if (s.user_code !== code) setCodeError(null);
      setCode(s.user_code);
    }
    if (!urlEditedRef.current && s.control_plane_url)
      setUrl(s.control_plane_url);
    if (formState)
      setFormError({
        text: describeError(s.link_error),
        detail: s.link_error_detail || undefined,
      });
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [status]);

  const view: View = useMemo(() => {
    const s = status;
    if (!s) return seen || graceDone ? "noagent" : "wait";
    // the federated flow renders its own progress instead of the code view
    if (fed) return "fed";
    if (s.state === "linking" && s.user_code) return "code";
    if (started && LINKED.includes(s.state)) return "done";
    if (forceForm || formState) return "form";
    if (LINKED.includes(s.state)) return "linked";
    return "noagent";
  }, [status, seen, graceDone, started, forceForm, formState, fed]);

  // auto-close the "Linked" view on the transition INTO online - status
  // still says "online" while a re-link is being set up (and after Cancel),
  // so arming on any done+online render would self-close under a pending
  // link; keying on a code->done view transition misses because approval
  // emits connecting (LINKED) before online
  const prevState = useRef<string | undefined>(undefined);
  useEffect(() => {
    const was = prevState.current;
    prevState.current = status?.state;
    if (view === "done" && status?.state === "online") {
      if (
        was !== undefined &&
        was !== "online" &&
        closeTimer.current === undefined
      ) {
        closeTimer.current = window.setTimeout(
          () => void invoke("close_window"),
          4000,
        );
      }
    } else if (closeTimer.current !== undefined) {
      clearTimeout(closeTimer.current);
      closeTimer.current = undefined;
    }
  }, [view, status]);

  const effectiveUrl =
    urlEdited || url ? url : status?.control_plane_url || defaults?.url || "";
  // varde.games sign-in only exists on the hosted control plane - and not
  // in the elevated re-link window: a browser callback lands in a normal
  // instance that the agent refuses to re-link, so re-link uses the code
  // flow (browser-side approval) only
  const fedEligible = !defaults?.relink;
  const hosted = hostOf(effectiveUrl) === HOSTED_CP_HOST && fedEligible;
  const backToHosted = () => {
    // counts as a user edit: otherwise the status handler would restore a
    // remembered self-hosted address and hide sign-in again
    urlEditedRef.current = true;
    setUrlEdited(true);
    setUrl(HOSTED_CP_URL);
    setServerOpen(false);
  };

  const doLink = useCallback(async (u: string) => {
    setFormError(null);
    setLinkBusy(true);
    try {
      setStatus(await invoke<UiStatus>("start_link", { url: u }));
    } catch (e) {
      setStarted(false);
      setFormError(errorParts(e));
    } finally {
      setLinkBusy(false);
    }
  }, []);

  const service = useCallback(
    async (
      cmd: "start_service" | "stop_service",
      key: string,
      setErr: (m: ErrParts | null) => void,
    ) => {
      setErr(null);
      setBusy((b) => ({ ...b, [key]: true }));
      try {
        // the elevated instance does the work; status arrives on its own
        await invoke(cmd);
        setTimeout(() => setBusy((b) => ({ ...b, [key]: false })), 4000);
      } catch (e) {
        setBusy((b) => ({ ...b, [key]: false }));
        setErr(errorParts(e));
      }
    },
    [],
  );

  const copy = useCallback(async () => {
    try {
      await navigator.clipboard.writeText(code);
    } catch {
      const r = document.createRange();
      const el = document.getElementById("link-code");
      if (el) {
        r.selectNodeContents(el);
        getSelection()?.removeAllRanges();
        getSelection()?.addRange(r);
        document.execCommand("copy");
      }
    }
    setCopied(true);
    setTimeout(() => setCopied(false), 1500);
  }, [code]);

  const cancelLink = useCallback(async () => {
    setCodeError(null);
    try {
      await invoke("cancel_link");
    } catch (e) {
      setCodeError(errorParts(e));
      return;
    }
    setStarted(false);
    setForceForm(true);
  }, []);

  const beginVardeLogin = useCallback(async () => {
    setFormError(null);
    try {
      await invoke("begin_varde_login");
    } catch (e) {
      const p = errorParts(e);
      setFed({ stage: "error", message: p.text, detail: p.detail });
    }
  }, []);

  // installer launch (--link): start linking as soon as not_linked shows -
  // varde.games sign-in on the hosted control plane, code flow elsewhere.
  // Decide off the status's own address: url/defaults can still hold the
  // hosted fallback when the first status lands.
  useEffect(() => {
    if (view === "form" && autoPending && status?.state === "not_linked") {
      setAutoPending(false);
      const autoUrl = status.control_plane_url || effectiveUrl;
      if (hostOf(autoUrl) === HOSTED_CP_HOST && !defaults?.relink)
        void beginVardeLogin();
      else void doLink(autoUrl);
    }
  }, [
    view,
    autoPending,
    status,
    defaults,
    effectiveUrl,
    doLink,
    beginVardeLogin,
  ]);

  const cancelFed = useCallback(() => {
    // clears the stored token/group pick and any mid-flight link
    void invoke("cancel_varde_login").catch(() => {});
    setFed(null);
  }, []);

  const finishVardeLogin = useCallback(async () => {
    try {
      await invoke("finish_varde_login", { groupId: fedGroup });
    } catch (e) {
      const p = errorParts(e);
      setFed({ stage: "error", message: p.text, detail: p.detail });
    }
  }, [fedGroup]);

  const confirmVardeLogin = useCallback(async () => {
    try {
      await invoke("confirm_varde_login");
    } catch (e) {
      const p = errorParts(e);
      setFed({ stage: "error", message: p.text, detail: p.detail });
    }
  }, []);

  const expiry = (() => {
    if (!status || status.state !== "linking" || !status.expires_at_unix_ms)
      return "";
    const left = Math.max(
      0,
      Math.round((status.expires_at_unix_ms - now) / 1000),
    );
    const m = Math.floor(left / 60);
    const sec = String(left % 60).padStart(2, "0");
    return `Code expires in ${m}:${sec}`;
  })();

  // only the elevated --relink window can cancel an administrator's re-link
  const locked = !!status?.relink && !defaults?.relink;

  return (
    <div className="h-full overflow-y-auto px-7 py-6 text-foreground">
      <header className="mb-7 flex items-center">
        <Logo className="h-7" />
      </header>
      <main>
        {view === "wait" && (
          <section className="vd-fade flex items-center gap-3 text-sm text-muted">
            <Mark state="moving" className="h-7 w-7 text-foreground" />
            Starting Varde...
          </section>
        )}

        {view === "form" && (
          <section className="vd-enter">
            <h1 className="font-display text-xl font-semibold tracking-tight">
              {defaults?.relink ? "Re-link this PC" : "Set up this PC"}
            </h1>
            <p className="mt-2.5 text-sm leading-relaxed">
              {defaults?.relink
                ? "Varde opens your dashboard so you can approve this PC. Sign up there, then create a group or join your friends with an invite link."
                : "Sign in with your varde.games account to link this PC to a group. Running your own server? Use the self-hosted option instead."}
            </p>
            {defaults?.relink && (
              <WarnNote>
                This PC leaves its current group and gets a new identity.
              </WarnNote>
            )}
            <form
              className="mt-3.5 flex flex-col gap-3.5"
              onSubmit={(e) => {
                e.preventDefault();
                void doLink(effectiveUrl.trim());
              }}
            >
              {serverOpen ? (
                <Field
                  label={
                    fedEligible ? "Self-hosted Varde server" : "Varde server"
                  }
                  help={
                    fedEligible
                      ? "The URL of your group's control plane."
                      : "Only if your group runs its own Varde, for example a friend's."
                  }
                >
                  <TextField
                    type="url"
                    isRequired
                    className="w-full"
                    onChange={(v) => {
                      urlEditedRef.current = true;
                      setUrl(v);
                      setUrlEdited(true);
                    }}
                    value={url}
                  >
                    <Input
                      spellCheck={false}
                      autoComplete="url"
                      placeholder="https://cp.example.com"
                    />
                  </TextField>
                </Field>
              ) : !hosted ? (
                <div className="flex flex-wrap items-baseline gap-x-2.5 gap-y-1 text-sm">
                  <span>Server: {hostOf(effectiveUrl) || HOSTED_CP_HOST}</span>
                  <button
                    type="button"
                    className="text-accent underline-offset-4 transition-colors hover:underline"
                    onClick={() => setServerOpen(true)}
                  >
                    Use a different server
                  </button>
                </div>
              ) : null}
              <ErrorNote detail={formError?.detail}>
                {formError?.text}
              </ErrorNote>
              {serverOpen ? (
                <div className="mt-2 flex flex-wrap gap-2">
                  <Button variant="primary" type="submit" busy={linkBusy}>
                    Continue
                  </Button>
                  {fedEligible && (
                    <Button onClick={backToHosted}>
                      Use varde.games instead
                    </Button>
                  )}
                </div>
              ) : hosted ? (
                <>
                  <Button
                    variant="primary"
                    busy={linkBusy}
                    onClick={() => void beginVardeLogin()}
                  >
                    Continue with your Varde account
                  </Button>
                  <div className="flex flex-wrap gap-2">
                    <Button onClick={() => setServerOpen(true)}>
                      Use a self-hosted instance
                    </Button>
                    <Button variant="ghost" type="submit" busy={linkBusy}>
                      Use a code instead
                    </Button>
                  </div>
                </>
              ) : (
                <div className="mt-2">
                  <Button variant="primary" type="submit" busy={linkBusy}>
                    Continue
                  </Button>
                </div>
              )}
            </form>
          </section>
        )}

        {view === "fed" && fed && (
          <section className="vd-enter">
            <h1 className="font-display text-xl font-semibold tracking-tight">
              Log in with Varde
            </h1>
            {fed.stage === "confirm" && (
              <>
                <p className="mt-2.5 text-sm leading-relaxed">
                  varde.games wants to link this PC to{" "}
                  {fed.email ? <strong>{fed.email}</strong> : "your account"}.
                </p>
                <p className="mt-2 text-sm text-muted">
                  Only continue if you just clicked Log in with Varde.
                </p>
              </>
            )}
            {fed.stage === "waiting_browser" && (
              <>
                <p className="mt-2.5 text-sm leading-relaxed">
                  Finish signing in at varde.games - you'll come back here
                  automatically.
                </p>
                <p className="mt-2 flex items-center gap-2 text-sm text-muted">
                  <Spinner size="sm" /> Waiting for your browser...
                </p>
              </>
            )}
            {fed.stage === "exchanging" && (
              <p className="mt-2.5 flex items-center gap-2 text-sm leading-relaxed">
                <Spinner size="sm" /> Signing you in...
              </p>
            )}
            {fed.stage === "approving" && (
              <p className="mt-2.5 flex items-center gap-2 text-sm leading-relaxed">
                <Spinner size="sm" /> Linking this PC...
              </p>
            )}
            {fed.stage === "pick_group" && (
              <>
                <p className="mt-2.5 text-sm leading-relaxed">
                  Pick the group this PC joins.
                </p>
                <Select
                  selectedKey={fedGroup}
                  onSelectionChange={(k) => setFedGroup(String(k))}
                  className="mt-3.5 w-full"
                >
                  <Label>Group</Label>
                  <Select.Trigger>
                    <Select.Value />
                    <Select.Indicator />
                  </Select.Trigger>
                  <Select.Popover>
                    <ListBox>
                      {fed.groups.map((g) => (
                        <ListBox.Item key={g.id} id={g.id}>
                          {g.name}
                        </ListBox.Item>
                      ))}
                    </ListBox>
                  </Select.Popover>
                </Select>
              </>
            )}
            {fed.stage === "error" && (
              <ErrorNote detail={fed.detail}>{fed.message}</ErrorNote>
            )}
            <div className="mt-3.5 flex flex-wrap gap-2">
              {fed.stage === "confirm" && (
                <Button
                  variant="primary"
                  onClick={() => void confirmVardeLogin()}
                >
                  Link this PC
                </Button>
              )}
              {fed.stage === "pick_group" && (
                <Button
                  variant="primary"
                  disabled={!fedGroup}
                  onClick={() => void finishVardeLogin()}
                >
                  Link this PC
                </Button>
              )}
              {fed.stage === "error" && (
                <Button
                  variant="primary"
                  onClick={() => void beginVardeLogin()}
                >
                  Try again
                </Button>
              )}
              <Button onClick={() => cancelFed()}>
                <CloseIcon /> Cancel
              </Button>
            </div>
          </section>
        )}

        {view === "code" && status && (
          <section className="vd-enter">
            <h1 className="font-display text-xl font-semibold tracking-tight">
              Approve this PC in your browser
            </h1>
            <p className="mt-2.5 text-sm leading-relaxed">
              Your browser opened Varde. Check that it shows this code, then
              approve:
            </p>
            <div
              id="link-code"
              aria-live="polite"
              className="mt-1.5 mb-3 select-all rounded-lg border border-separator bg-surface-secondary px-3 py-4 text-center font-mono text-4xl font-semibold tracking-widest"
            >
              {code}
            </div>
            <p className="text-sm text-muted">{expiry}</p>
            {locked && (
              <p className="mt-1 text-sm text-muted">
                An administrator is re-linking this PC.
              </p>
            )}
            <ErrorNote detail={codeError?.detail}>{codeError?.text}</ErrorNote>
            <div className="mt-3.5 flex flex-wrap gap-2">
              <Button onClick={() => void copy()}>
                <CopyIcon /> {copied ? "Copied" : "Copy code"}
              </Button>
              <Button
                onClick={() =>
                  void invoke("open_url", { url: status.link_url })
                }
              >
                <OpenIcon /> Open again
              </Button>
              {!locked && (
                <Button variant="ghost" onClick={() => void cancelLink()}>
                  <CloseIcon /> Cancel
                </Button>
              )}
            </div>
          </section>
        )}

        {view === "done" && status && (
          <section className="vd-enter">
            <h1 className="font-display text-xl font-semibold tracking-tight">
              Linked
            </h1>
            <p className="mt-2.5 text-sm leading-relaxed">{describe(status)}</p>
            <p className="mt-2 text-sm text-muted">
              The Varde service is running on this PC.
            </p>
            <div className="mt-3.5 flex flex-wrap gap-2">
              <Button
                variant="primary"
                onClick={() => void invoke("close_window")}
              >
                Close
              </Button>
            </div>
          </section>
        )}

        {view === "linked" && status && (
          <section className="vd-enter">
            <h1 className="font-display text-xl font-semibold tracking-tight">
              This PC is linked
            </h1>
            <p className="mt-2.5 text-sm leading-relaxed">{describe(status)}</p>
            <p className="mt-2 text-sm text-muted">
              The Varde service is running on this PC.
            </p>
            {status.save_line && (
              <p className="mt-2 text-sm text-muted">{status.save_line}</p>
            )}
            <ErrorNote detail={linkedError?.detail}>
              {linkedError?.text}
            </ErrorNote>
            <div className="mt-3.5 flex flex-wrap gap-2">
              <Button
                variant="primary"
                onClick={() => {
                  setLinkedError(null);
                  void invoke("open_dashboard").catch((e) =>
                    setLinkedError(errorParts(e)),
                  );
                }}
              >
                <DashboardIcon /> Open dashboard
              </Button>
              <Button
                variant="ghost"
                onClick={() =>
                  void invoke("open_url", { url: status.control_plane_url })
                }
              >
                <OpenIcon /> Open in browser
              </Button>
              {/* stopping while hosting would leave the game running unmanaged */}
              {!status.hosting && (
                <Button
                  busy={busy["svc-stop"]}
                  onClick={() =>
                    void service("stop_service", "svc-stop", setLinkedError)
                  }
                >
                  <PowerIcon />{" "}
                  {busy["svc-stop"] ? "Stopping..." : "Stop Varde"}
                </Button>
              )}
              <Button
                variant="ghost"
                onClick={() => void invoke("close_window")}
              >
                <CloseIcon /> Close
              </Button>
            </div>
          </section>
        )}

        {view === "noagent" && (
          <section className="vd-enter">
            <h1 className="font-display text-xl font-semibold tracking-tight">
              Varde isn't running
            </h1>
            <WarnNote>
              <strong>Service unreachable.</strong> The Varde service on this PC
              isn't answering. Varde can't host or join servers until it runs
              again.
            </WarnNote>
            <p className="mt-3.5 text-sm text-muted">
              It normally starts with Windows. Starting it needs administrator
              approval once.
            </p>
            <ErrorNote detail={noagentError?.detail}>
              {noagentError?.text}
            </ErrorNote>
            <div className="mt-3.5 flex flex-wrap gap-2">
              <Button
                variant="primary"
                busy={busy["svc-start"]}
                onClick={() =>
                  void service("start_service", "svc-start", setNoagentError)
                }
              >
                <PowerIcon />{" "}
                {busy["svc-start"] ? "Starting..." : "Start Varde"}
              </Button>
              <Button
                variant="ghost"
                onClick={() => void invoke("close_window")}
              >
                <CloseIcon /> Close
              </Button>
            </div>
          </section>
        )}
      </main>
    </div>
  );
}
