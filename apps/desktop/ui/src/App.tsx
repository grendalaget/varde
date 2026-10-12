// Link / status window. Talks only to the tray process (typed commands),
// which talks to the local Varde service.

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import {
  Button,
  Dropdown,
  Field,
  Input,
  Link,
  MessageBar,
  MessageBarBody,
  MessageBarTitle,
  Option,
  Text,
  Title2,
  makeStyles,
  tokens,
} from "@fluentui/react-components";
import {
  Board20Regular,
  Copy20Regular,
  Dismiss20Regular,
  Open20Regular,
  Power20Regular,
} from "@fluentui/react-icons";
import {
  invoke,
  listen,
  type FedState,
  type LinkDefaults,
  type UiStatus,
} from "./tauri";
import markUrl from "./assets/mark.svg";

const LINKED = ["connecting", "online", "offline"];
const brandFont =
  "'Inter Variable', 'Inter', 'Segoe UI', system-ui, sans-serif";

const hostOf = (u: string) => u.replace(/^[a-z]+:\/\//i, "").split("/")[0];

// the hosted control plane: only it can verify varde.games sign-in tokens
const HOSTED_CP_HOST = "cp.varde.games";
const HOSTED_CP_URL = "https://cp.varde.games";

const useStyles = makeStyles({
  page: {
    boxSizing: "border-box",
    height: "100%",
    padding: "24px 28px",
    backgroundColor: tokens.colorNeutralBackground1,
    color: tokens.colorNeutralForeground1,
    overflowY: "auto",
  },
  header: {
    display: "flex",
    alignItems: "center",
    gap: "10px",
    marginBottom: "28px",
  },
  mark: {
    width: "28px",
    height: "28px",
    // the asset is a single-color path; masking keeps it visible in both
    // light and dark themes (it inherits the themed foreground color)
    backgroundColor: tokens.colorNeutralForeground1,
    maskImage: `url("${markUrl}")`,
    maskSize: "contain",
  },
  brand: {
    fontFamily: brandFont,
    fontSize: "20px",
    fontWeight: 600,
    letterSpacing: "-0.012em",
  },
  title: {
    display: "block",
    fontFamily: brandFont,
    marginTop: 0,
    marginBottom: "10px",
  },
  lead: { display: "block", marginBottom: "14px" },
  muted: {
    display: "block",
    color: tokens.colorNeutralForeground3,
    marginBottom: "14px",
  },
  bar: { marginBottom: "14px" },
  code: {
    fontFamily: "Consolas, ui-monospace, monospace",
    fontSize: "34px",
    fontWeight: 600,
    lineHeight: 1.2,
    letterSpacing: "0.06em",
    backgroundColor: tokens.colorNeutralBackground2,
    border: `1px solid ${tokens.colorNeutralStroke1}`,
    borderRadius: "8px",
    padding: "18px 12px",
    textAlign: "center",
    marginTop: "6px",
    marginBottom: "12px",
    userSelect: "all",
  },
  row: {
    display: "flex",
    gap: "8px",
    flexWrap: "wrap",
    marginTop: "8px",
  },
  form: {
    marginTop: "14px",
    display: "flex",
    flexDirection: "column",
    gap: "14px",
  },
  serverRow: {
    display: "flex",
    alignItems: "baseline",
    gap: "10px",
    flexWrap: "wrap",
  },
});

type View = "wait" | "form" | "fed" | "code" | "done" | "linked" | "noagent";

function describe(s: UiStatus) {
  if (s.group_name && s.node_name)
    return `Linked to ${s.group_name} as ${s.node_name}.`;
  if (s.group_name) return `Linked to ${s.group_name}.`;
  return "Linked. Connecting to Varde…";
}

export default function App() {
  const styles = useStyles();
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
  const [formError, setFormError] = useState("");
  const [codeError, setCodeError] = useState("");
  const [linkedError, setLinkedError] = useState("");
  const [noagentError, setNoagentError] = useState("");
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
      // subscribed — hydrate the current stage
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
    // a federated sign-in is done once ITS link flow reaches the service —
    // during a re-link the old link still reports a LINKED state until the
    // new one is approved, so wait for our own link to have started
    if (started && LINKED.includes(s.state)) setFed(null);
    if (s.state === "linking" && s.user_code) {
      setStarted(true);
      // a new code: drop a refusal shown for the previous one
      if (s.user_code !== code) setCodeError("");
      setCode(s.user_code);
    }
    if (!urlEditedRef.current && s.control_plane_url)
      setUrl(s.control_plane_url);
    if (formState) setFormError(s.link_error);
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

  // auto-close the "Linked" view on the transition INTO online — status
  // still says "online" while a re-link is being set up (and after Cancel),
  // so arming on any done+online render would self-close under a pending
  // link; keying on a code→done view transition misses because approval
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
  // varde.games sign-in only exists on the hosted control plane — and not
  // in the elevated re-link window: a browser callback lands in a normal
  // instance that the agent refuses to re-link, so re-link uses the code
  // flow (browser-side approval) only
  const fedEligible = !defaults?.relink;
  const hosted = hostOf(effectiveUrl) === HOSTED_CP_HOST && fedEligible;
  const backToHosted = () => {
    urlEditedRef.current = false;
    setUrlEdited(false);
    setUrl(HOSTED_CP_URL);
    setServerOpen(false);
  };

  const doLink = useCallback(async (u: string) => {
    setFormError("");
    setLinkBusy(true);
    try {
      setStatus(await invoke<UiStatus>("start_link", { url: u }));
    } catch (e) {
      setStarted(false);
      setFormError(String(e));
    } finally {
      setLinkBusy(false);
    }
  }, []);

  // installer launch (--link): start linking as soon as not_linked shows
  useEffect(() => {
    if (view === "form" && autoPending && status?.state === "not_linked") {
      setAutoPending(false);
      void doLink(effectiveUrl);
    }
  }, [view, autoPending, status, effectiveUrl, doLink]);

  const service = useCallback(
    async (
      cmd: "start_service" | "stop_service",
      key: string,
      setErr: (m: string) => void,
    ) => {
      setErr("");
      setBusy((b) => ({ ...b, [key]: true }));
      try {
        // the elevated instance does the work; status arrives on its own
        await invoke(cmd);
        setTimeout(() => setBusy((b) => ({ ...b, [key]: false })), 4000);
      } catch (e) {
        setBusy((b) => ({ ...b, [key]: false }));
        setErr(String(e));
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
    setCodeError("");
    try {
      await invoke("cancel_link");
    } catch (e) {
      setCodeError(String(e));
      return;
    }
    setStarted(false);
    setForceForm(true);
  }, []);

  const beginVardeLogin = useCallback(async () => {
    setFormError("");
    try {
      await invoke("begin_varde_login");
    } catch (e) {
      setFed({ stage: "error", message: String(e) });
    }
  }, []);

  const cancelFed = useCallback(() => {
    // clears the stored token/group pick and any mid-flight link
    void invoke("cancel_varde_login").catch(() => {});
    setFed(null);
  }, []);

  const finishVardeLogin = useCallback(async () => {
    try {
      await invoke("finish_varde_login", { groupId: fedGroup });
    } catch (e) {
      setFed({ stage: "error", message: String(e) });
    }
  }, [fedGroup]);

  const confirmVardeLogin = useCallback(async () => {
    try {
      await invoke("confirm_varde_login");
    } catch (e) {
      setFed({ stage: "error", message: String(e) });
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
    <div className={styles.page}>
      <header className={styles.header}>
        <div className={styles.mark} />
        <span className={styles.brand}>varde</span>
      </header>
      <main>
        {view === "form" && (
          <section>
            <Title2 className={styles.title} as="h1">
              {defaults?.relink ? "Re-link this PC" : "Set up this PC"}
            </Title2>
            <Text className={styles.lead} as="p">
              {defaults?.relink
                ? "Varde opens your dashboard so you can approve this PC. Sign up there, then create a group or join your friends with an invite link."
                : "Sign in with your varde.games account to link this PC to a group. Running your own server? Use the self-hosted option instead."}
            </Text>
            {defaults?.relink && (
              <MessageBar intent="warning" className={styles.bar}>
                <MessageBarBody>
                  This PC leaves its current group and gets a new identity.
                </MessageBarBody>
              </MessageBar>
            )}
            <form
              className={styles.form}
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
                  hint={
                    fedEligible
                      ? "The URL of your group's control plane."
                      : "Only if your group runs its own Varde, for example a friend's."
                  }
                >
                  <Input
                    type="url"
                    required
                    spellCheck={false}
                    autoComplete="url"
                    value={url}
                    onChange={(_, d) => {
                      urlEditedRef.current = true;
                      setUrl(d.value);
                      setUrlEdited(true);
                    }}
                  />
                </Field>
              ) : !hosted ? (
                <div className={styles.serverRow}>
                  <Text as="span">
                    Server: {hostOf(effectiveUrl) || HOSTED_CP_HOST}
                  </Text>
                  <Link as="button" onClick={() => setServerOpen(true)}>
                    Use a different server
                  </Link>
                </div>
              ) : null}
              {formError && (
                <MessageBar intent="error" className={styles.bar}>
                  <MessageBarBody>{formError}</MessageBarBody>
                </MessageBar>
              )}
              {serverOpen ? (
                <div className={styles.row}>
                  <Button
                    appearance="primary"
                    type="submit"
                    disabled={linkBusy}
                  >
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
                    appearance="primary"
                    disabled={linkBusy}
                    onClick={() => void beginVardeLogin()}
                  >
                    Continue with your Varde account
                  </Button>
                  <div className={styles.row}>
                    <Button onClick={() => setServerOpen(true)}>
                      Use a self-hosted instance
                    </Button>
                    <Button
                      type="submit"
                      appearance="subtle"
                      disabled={linkBusy}
                    >
                      Use a code instead
                    </Button>
                  </div>
                </>
              ) : (
                <div>
                  <Button
                    appearance="primary"
                    type="submit"
                    disabled={linkBusy}
                  >
                    Continue
                  </Button>
                </div>
              )}
            </form>
          </section>
        )}

        {view === "fed" && fed && (
          <section>
            <Title2 className={styles.title} as="h1">
              Log in with Varde
            </Title2>
            {fed.stage === "confirm" && (
              <>
                <Text className={styles.lead} as="p">
                  varde.games wants to link this PC to{" "}
                  {fed.email ? <strong>{fed.email}</strong> : "your account"}.
                </Text>
                <Text className={styles.muted} as="p">
                  Only continue if you just clicked Log in with Varde.
                </Text>
              </>
            )}
            {fed.stage === "waiting_browser" && (
              <>
                <Text className={styles.lead} as="p">
                  Finish signing in at varde.games — you'll come back here
                  automatically.
                </Text>
                <Text className={styles.muted} as="p">
                  Waiting for your browser…
                </Text>
              </>
            )}
            {fed.stage === "exchanging" && (
              <Text className={styles.lead} as="p">
                Signing you in…
              </Text>
            )}
            {fed.stage === "approving" && (
              <Text className={styles.lead} as="p">
                Linking this PC…
              </Text>
            )}
            {fed.stage === "pick_group" && (
              <>
                <Text className={styles.lead} as="p">
                  Pick the group this PC joins.
                </Text>
                <Field label="Group">
                  <Dropdown
                    value={
                      fed.groups.find((g) => g.id === fedGroup)?.name ?? ""
                    }
                    selectedOptions={fedGroup ? [fedGroup] : []}
                    onOptionSelect={(_, d) =>
                      setFedGroup(String(d.optionValue ?? ""))
                    }
                  >
                    {fed.groups.map((g) => (
                      <Option key={g.id} value={g.id}>
                        {g.name}
                      </Option>
                    ))}
                  </Dropdown>
                </Field>
              </>
            )}
            {fed.stage === "error" && (
              <MessageBar intent="error" className={styles.bar}>
                <MessageBarBody>{fed.message}</MessageBarBody>
              </MessageBar>
            )}
            <div className={styles.row}>
              {fed.stage === "confirm" && (
                <Button
                  appearance="primary"
                  onClick={() => void confirmVardeLogin()}
                >
                  Link this PC
                </Button>
              )}
              {fed.stage === "pick_group" && (
                <Button
                  appearance="primary"
                  disabled={!fedGroup}
                  onClick={() => void finishVardeLogin()}
                >
                  Link this PC
                </Button>
              )}
              {fed.stage === "error" && (
                <Button
                  appearance="primary"
                  onClick={() => void beginVardeLogin()}
                >
                  Try again
                </Button>
              )}
              <Button icon={<Dismiss20Regular />} onClick={() => cancelFed()}>
                Cancel
              </Button>
            </div>
          </section>
        )}

        {view === "code" && status && (
          <section>
            <Title2 className={styles.title} as="h1">
              Approve this PC in your browser
            </Title2>
            <Text className={styles.lead} as="p">
              Your browser opened Varde. Check that it shows this code, then
              approve:
            </Text>
            <div id="link-code" className={styles.code} aria-live="polite">
              {code}
            </div>
            <Text className={styles.muted} as="p">
              {expiry}
            </Text>
            {locked && (
              <Text className={styles.muted} as="p">
                An administrator is re-linking this PC.
              </Text>
            )}
            {codeError && (
              <MessageBar intent="error" className={styles.bar}>
                <MessageBarBody>{codeError}</MessageBarBody>
              </MessageBar>
            )}
            <div className={styles.row}>
              <Button icon={<Copy20Regular />} onClick={() => void copy()}>
                {copied ? "Copied" : "Copy code"}
              </Button>
              <Button
                icon={<Open20Regular />}
                onClick={() =>
                  void invoke("open_url", { url: status.link_url })
                }
              >
                Open again
              </Button>
              {!locked && (
                <Button
                  icon={<Dismiss20Regular />}
                  onClick={() => void cancelLink()}
                >
                  Cancel
                </Button>
              )}
            </div>
          </section>
        )}

        {view === "done" && status && (
          <section>
            <Title2 className={styles.title} as="h1">
              Linked
            </Title2>
            <Text className={styles.lead} as="p">
              {describe(status)}
            </Text>
            <Text className={styles.muted} as="p">
              The Varde service is running on this PC.
            </Text>
            <div className={styles.row}>
              <Button
                appearance="primary"
                onClick={() => void invoke("close_window")}
              >
                Close
              </Button>
            </div>
          </section>
        )}

        {view === "linked" && status && (
          <section>
            <Title2 className={styles.title} as="h1">
              This PC is linked
            </Title2>
            <Text className={styles.lead} as="p">
              {describe(status)}
            </Text>
            <Text className={styles.muted} as="p">
              The Varde service is running on this PC.
            </Text>
            {linkedError && (
              <MessageBar intent="error" className={styles.bar}>
                <MessageBarBody>{linkedError}</MessageBarBody>
              </MessageBar>
            )}
            <div className={styles.row}>
              <Button
                appearance="primary"
                icon={<Board20Regular />}
                onClick={() => {
                  setLinkedError("");
                  void invoke("open_dashboard").catch((e) =>
                    setLinkedError(String(e)),
                  );
                }}
              >
                Open dashboard
              </Button>
              <Button
                appearance="subtle"
                icon={<Open20Regular />}
                onClick={() =>
                  void invoke("open_url", { url: status.control_plane_url })
                }
              >
                Open in browser
              </Button>
              {/* stopping while hosting would leave the game running unmanaged */}
              {!status.hosting && (
                <Button
                  icon={<Power20Regular />}
                  disabled={busy["svc-stop"]}
                  onClick={() =>
                    void service("stop_service", "svc-stop", setLinkedError)
                  }
                >
                  {busy["svc-stop"] ? "Stopping…" : "Stop Varde"}
                </Button>
              )}
              <Button
                icon={<Dismiss20Regular />}
                onClick={() => void invoke("close_window")}
              >
                Close
              </Button>
            </div>
          </section>
        )}

        {view === "noagent" && (
          <section>
            <Title2 className={styles.title} as="h1">
              Varde isn't running
            </Title2>
            <MessageBar intent="warning" className={styles.bar}>
              <MessageBarBody>
                <MessageBarTitle>Service unreachable</MessageBarTitle>
                The Varde service on this PC isn't answering. Varde can't host
                or join servers until it runs again.
              </MessageBarBody>
            </MessageBar>
            <Text className={styles.muted} as="p">
              It normally starts with Windows. Starting it needs administrator
              approval once.
            </Text>
            {noagentError && (
              <MessageBar intent="error" className={styles.bar}>
                <MessageBarBody>{noagentError}</MessageBarBody>
              </MessageBar>
            )}
            <div className={styles.row}>
              <Button
                appearance="primary"
                icon={<Power20Regular />}
                disabled={busy["svc-start"]}
                onClick={() =>
                  void service("start_service", "svc-start", setNoagentError)
                }
              >
                {busy["svc-start"] ? "Starting…" : "Start Varde"}
              </Button>
              <Button
                icon={<Dismiss20Regular />}
                onClick={() => void invoke("close_window")}
              >
                Close
              </Button>
            </div>
          </section>
        )}
      </main>
    </div>
  );
}
